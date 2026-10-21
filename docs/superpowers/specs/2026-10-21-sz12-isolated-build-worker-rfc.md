# SZ-12: isolated build worker — architecture RFC

Status: **proposed, not approved for implementation**. Design and documentation
only; no application code in this document's change set.

Base: `integration/sz11b-candidate` @ `283ac4fbe5c6244fb43bf42138c2c32fae68257e`.

Companion evidence file: `docs/superpowers/specs/2026-10-21-sz12-build-worker-feasibility.md`
("the feasibility file"). Every factual claim in this RFC is tagged:

| Tag | Meaning |
| --- | --- |
| **[M]** | Measured in the feasibility file on a disposable machine. Reproducible command included there. |
| **[C]** | Read directly from the StageZero source at `283ac4f`; file and line given. |
| **[D]** | Design intent in this RFC. Not implemented, not measured. Must pass a feasibility gate before it may be relied on. |

Railpack version under discussion: **0.37.0**, matching the pin in
`api/jobs/deploy.go` and the API Dockerfile. No compatibility with any other
Railpack version is claimed.

## 0. Executive summary

StageZero executes repository content in two places, and neither is a security
boundary:

1. `source.Prepare` runs the **Railpack binary on the API host** against
   untrusted repository content [C: `api/source/source.go:669`], once during
   `POST /api/source/analyze` [C: `api/handlers/source.go:26`] and again
   during every source deployment [C: `api/jobs/deploy.go:276-300`].
2. `docker buildx build` runs **on the customer's target server** over SSH
   [C: `api/jobs/deploy.go:415-454`], with the repository as build context and
   build-time secrets mounted into the build [C: `api/jobs/deploy.go:431-433`].

The API host holds the platform's master key, the database credential, OAuth
client secrets and every workspace's SSH private keys, all under one global
AES-256-GCM key with no key versioning [C: `api/crypto/crypto.go:19-36`,
`api/config/config.go:31`]. A parser or execution bug in the preparation step
is therefore a full control-plane compromise, not a nuisance.

This RFC proposes a **separate build worker host** — separate kernel, separate
network zone, no credentials, no route to the control plane except one
authenticated job channel — that performs source acquisition, preparation and
image building. The API validates the plan the worker returns, pins every image
to a digest the API itself resolved, and streams the resulting artifact to the
customer's target. The customer target keeps its current role; it receives an
image instead of building one.

Three findings materially shape the design and are measured, not assumed:

- **The Railpack plan is repository-controlled.** A `railpack.json` inside the
  repository injects arbitrary shell commands into the plan's build step and
  into `deploy.startCommand`, and changes the toolchain, package manager, base
  image and cache set [M: F9]. Therefore plan validation is a **control-object
  mechanism** (images, structure, secrets, inputs) and can never be a
  safety mechanism. The sandbox is the boundary.
- **The stock `docker buildx create --driver docker-container` produces a
  `Privileged=true` builder container**, and `buildx create` exposes no
  capability-drop, read-only-rootfs, pids-limit, memory or user knobs [M: F1].
  Revision 2's "builder not privileged, `--cap-drop ALL`" claim is therefore
  **withdrawn**. The recommended worker runs a **native rootless `buildkitd`**
  with no Docker daemon on the host at all [M: F3, untested for build steps on
  this nested machine].
- **Docker network separation, not publish binding, is the control that
  matters.** A container published on `127.0.0.1` is still fully reachable by
  every container on the same Docker network [M: F5, cases A and H3], while
  traffic between two user-defined networks is dropped [M: F5, cases D and H2].
  The monitoring agent is therefore reachable from every application container
  today, and the effective fix is a separate network, not a tighter publish.

## 1. Problem, goals, non-goals

### 1.1 Problem

Repository content is untrusted input that today executes in the control plane
and on customer infrastructure. The product's own documentation already names
the missing boundary: "the later isolated build-worker boundary for untrusted
repositories is a separate release gate" [C:
`docs/superpowers/specs/2026-10-17-sz09-import-railpack-verification.md:5`],
and records that `railpack prepare` "is **not** confined to its work directory
or restricted to git-only network access".

### 1.2 Goals

| ID | Goal |
| --- | --- |
| G1 | Untrusted repository content never executes in the control plane. |
| G2 | Untrusted repository content never executes on a customer target. |
| G3 | A compromised build worker cannot reach the control plane, the database, the session store, the SSH keys, or any customer target. |
| G4 | Build-time secrets are not present in the build environment by default. |
| G5 | The current integration candidate keeps working; the transition does not break production source deployments before worker capacity exists. |
| G6 | After activation, no automatic fallback can re-enable the vulnerable path. |

### 1.3 Non-goals

| ID | Non-goal | Why |
| --- | --- | --- |
| N1 | Hardening the runtime container of the deployed application. | Separate risk class (T6/T9); tracked as SZ-13, see §4.10. |
| N2 | Fixing the missing SSH host-key verification (`InsecureIgnoreHostKey` [C: `api/ssh/client.go:94`]). | Real P0, but orthogonal; a control-plane change, not a build-worker change. Recommended as its own P0 (see §14, B4). |
| N3 | The `goose down` safety guard. | Already tracked separately; unrelated to builds. |
| N4 | Multi-tenant scheduling, autoscaling, attestation signing. | Deferred to Phase 2/3 (§11). |
| N5 | Replacing GitHub as the source provider. | The provider is constrained to `https://github.com` [C: `api/source/source.go:241`]; other providers are out of scope. |

## 2. Current state

### 2.1 Execution path (as implemented at `283ac4f`)

```
Browser
  │
  │  POST /api/source/analyze            POST /api/projects/from-source
  │  POST /api/deployments               (analysis runs again, synchronously)
  ▼
API host  ── CONTROL PLANE ─────────────────────────────────────────────┐
  │  ENCRYPTION_KEY (master AES-256-GCM key)                             │
  │  DATABASE_URL, GITHUB_CLIENT_SECRET, GOOGLE_CLIENT_SECRET            │
  │  every workspace's SSH private key (encrypted at rest)               │
  │  every service's env var values (encrypted at rest)                  │
  │                                                                       │
  ├─ source.AnalysisGate: 6/min per user, 2 concurrent, 90 s [C: api/source/limits.go]│
  ├─ ParseRepoURLWithBranch — github.com only, no userinfo [C: source.go:241]
  ├─ git ls-remote            (local exec on API host) [C: source.go:330]│
  ├─ source.Fetch             → codeload.github.com, public-IP-only dialer,│
  │                             no redirects, 500 MiB cap [C: source.go:44-86]│
  │      └─ extract → /tmp/stagezero-source-*  (traversal-checked)        │
  ├─ source.Prepare  ◀── UNTRUSTED CONTENT IS PARSED HERE, ON THE API HOST│
  │      railpack prepare <dir> --plan-out --env NAME                     │
  │      env allow-list only [C: source.go:672-679], 1 MiB output cap      │
  └─ jobs.RunDeploy (in-process goroutine) [C: handlers/deployments.go:118]│
         ├─ prepareSourcePlan  → source.Fetch + source.Prepare  (again!)   │
         │                      [C: jobs/deploy.go:276-300]                │
         ├─ ssh.NewClient  →  gossh.InsecureIgnoreHostKey()  [C: ssh/client.go:94]
         │  ┌──────────────── over SSH, script delivered on stdin ─────────┐│
         │  │ curl codeload tarball → /tmp/stagezero-source-<dep>/          ││
         │  │ railpack-plan.json uploaded FROM THE API HOST                 ││
         │  │ docker buildx build                                          ││
         │  │   --build-arg BUILDKIT_SYNTAX=ghcr.io/railwayapp/            ││
         │  │              railpack-frontend:v0.37.0                        ││
         │  │   -f railpack-plan.json                                      ││
         │  │   --build-arg cache-key=<svc> [--secret id=K,env=K]           ││
         │  │   --output type=docker,name=stagezero/<svc>:<sha>            ││
         │  │   <workdir>/<repo>-<sha>      ◀── REPOSITORY CONTENT IS        ││
         │  │                                    EXECUTED HERE, ON THE        ││
         │  │                                    CUSTOMER'S HOST              ││
         │  └──────────────────────────────────────────────────────────────┘│
         └─ docker run -d --network stagezero --env 'K=V' <image>          │
               ◀── runtime secrets on the target's argv [C: deploy.go:741]  │
                                                                          ┘
```

### 2.2 Current trust boundaries

| Zone | Contains | Trust |
| --- | --- | --- |
| **Control plane** (API host) | master key, DB, OAuth secrets, SSH keys, service env secrets, sessions | Fully trusted — and it *parses untrusted input* |
| **Customer target** | customer application, customer secrets, Docker daemon (root-equivalent for its user) | Trusted by contract, but it is customer infrastructure |
| **GitHub** | source of the untrusted content | External, partially trusted (read-only, pinned SHA) |

There is no zone boundary between untrusted content and the control plane. The
only controls are hygiene: the environment allow-list, the output cap, the
process-group kill, the tarball size caps, the public-IP-only dialer, and the
GitHub-only provider restriction.

### 2.3 Threat surface

| ID | Threat | Where | Owner |
| --- | --- | --- | --- |
| T1 | RCE in the control plane via a Railpack parsing/execution bug | `api/source/source.go:669` | SZ-12 |
| T2 | RCE on the customer target via the build | `api/jobs/deploy.go:415-454` | SZ-12 |
| T3 | Build-time secret exposure to untrusted build steps | `api/jobs/deploy.go:431-433` | SZ-12 (secrets off) + SZ-13 (runtime) |
| T4 | Runtime secrets visible in the target's process table | `api/jobs/deploy.go:741` | SZ-12 Phase 0 (out of argv) |
| T5 | SSH transport MITM (no host-key verification) | `api/ssh/client.go:94` | **Not SZ-12** (B4) |
| T6 | Application container → monitoring agent → Docker socket and host root filesystem | `api/monitoring/agent.go:216` | SZ-13 (network move in SZ-12 Phase 0) |
| T7 | Plan-controlled image references (repo-controlled, see F9) | plan | SZ-12 (digest pinning) |
| T8 | Cross-job BuildKit cache poisoning | build | SZ-12 (per-job builder) |
| T9 | Repository content reaches the runtime image (`deploy.inputs` copies `.`) | plan | SZ-13 (accepted, documented) |
| T10 | Untrusted build output reaching stored logs | `api/jobs/deploy.go:828-845` | SZ-12 (API-side redaction) |

## 3. Target architecture

### 3.1 Components

```
                    ┌──────────────────── CONTROL PLANE ─────────────────────┐
                    │                                                         │
  operator ────────►│  API (listener A, :8080)                              │
                    │   • owns ENCRYPTION_KEY, DATABASE_URL, OAuth secrets   │
                    │   • owns SSH keys, env secrets, sessions               │
                    │   • resolves source SHA (git ls-remote)                │
                    │   • validates + canonicalises the worker's plan        │
                    │   • resolves every image to a digest                   │
                    │   • stores artifacts, drives deployments                │
                    │                                                        │
                    │  GATEWAY (listener B, separate process, :8443)         │
                    │   • separate binary, separate credentials, no DB,       │
                    │     no ENCRYPTION_KEY, no SSH keys, no OAuth secrets    │
                    │   • 6 endpoints, worker-token auth only                 │
                    └───────────────┬────────────────────────────────────────┘
                                    │ mTLS + worker token (job-scoped)
                                    │ ▲ only permitted egress target
                    ════════════════╪═════════════════════════════════════
                       NETWORK ZONE BOUNDARY (deny-by-default, §5.8)
                    ════════════════╪═════════════════════════════════════
                                    ▼
              ┌───────────────── BUILD WORKER HOST (disposable) ─────────────┐
              │  worker agent (Go, non-root, systemd unit)                  │
              │   • no Docker daemon, no /var/run/docker.sock               │
              │   • per-job: own buildkitd (rootless), own tmpfs workdir,    │
              │     own unix socket, own label namespace                   │
              │  egress: gateway only + per-job provider allow-list         │
              │  no credentials of any kind, ever                          │
              └────────────────────────────────────────────────────────────┘
```

### 3.2 Target trust boundaries

| Zone | Trust | Contents |
| --- | --- | --- |
| **Control plane** | Trusted | All secrets, all tenant data, the plan validator, the image digest resolver, the artifact store. **Never parses repository content.** |
| **Gateway process** | Trusted-but-minimised | Worker token hashes, message plumbing. No database credential, no master key, no SSH key, no env secret. A gateway compromise yields the ability to impersonate a worker to the API — nothing more (§4.3). |
| **Worker host** | Untrusted for everything except build output | Repository content, the build, the artifact. No credentials, no route to the control plane except the gateway port. Rebuildable from an image at any time. |
| **Customer target** | Trusted by contract | Receives a finished image; keeps its current role. No longer executes repository content [G2]. |

### 3.3 Capability table — what the worker can reach

| Capability | Worker | Gateway process | API |
| --- | --- | --- | --- |
| Read repository content | Yes, only the assigned repo at the assigned SHA | No | No |
| Read workspace secrets / SSH keys / env values | **No** | **No** | Yes (decrypts on demand) |
| Read the database | **No** (no credential) | **No** | Yes |
| Reach the control plane other than the gateway port | **No** (egress deny-list) | Yes, listener A over a unix socket | — |
| Reach any customer target | **No** | **No** | Yes (SSH) |
| Write to the database | **No** | **No** | Yes |
| Mint a worker token | **No** | **No** | Yes (operator action) |
| Choose the images in a build | **No** (the API rewrites them to digests) | **No** | Yes |
| Change a deployment's terminal state | Only its own job, with a valid fence | No | Yes |

## 4. Resolution of the ten open inconsistencies

### 4.1 Source → isolated preparation → candidate plan → API validation → digest pinning → build

**Problem.** Revision 2 left the control plane preparing the repository and
shipping a plan to the worker, which contradicts "railpack never processes
repository content on the control-plane host".

**Resolution — the direction is inverted.** The control plane never sees
repository content. The worker is the only component that acquires and parses
it, and it does so *before* any plan exists.

```
1. API   resolve source coordinates
         git ls-remote → authoritative SHA for (owner, repo, branch)  [C: source.go:330]
         (read-only, no repository content is parsed)

2. API   enqueue build_job{kind: "prepare", source, deadline}

3. Worker  acquire source
          HTTPS GET codeload tarball at the API-supplied SHA (§5.8 policy P-A)
          compute SHA-256 of the tarball
          extract to the per-job tmpfs (existing traversal, symlink, entry
          and byte caps reused: api/source/source.go:521-628)

4. Worker  prepare in the sandbox  (repo content is parsed HERE, nowhere else)
          railpack prepare <tmpfs>/src --plan-out <tmpfs>/candidate.json
                       --info-out  <tmpfs>/info.json --hide-pretty-plan
          → candidate plan (repository-controlled! [M: F9])

5. Worker  POST /v1/jobs/{id}/plan  (candidate plan + info + tarball SHA-256
          + source SHA + railpack version + worker identity)

6. API    validate the candidate plan  (§5.2, structural allowlist)
         reject if: unknown top-level/step/command key; any image outside the
                     image policy; a secret list that is not empty; size > 1 MiB;
                     the plan's railpack version != 0.37.0; the reported source
                     SHA != the SHA the API resolved
         canonicalise: replace every image reference with a digest the API
                     resolved itself; empty every secret list; drop every
                     command's env/ssh/privileged-shaped fields (there are
                     none today [M: F6]; the policy rejects them if they appear)
         compute canonical plan SHA-256; store plan + digest on the job row
         → respond 200 {approved_plan, plan_sha256, resolved_images[]}

7. Worker  verify the approved plan's SHA-256, write it to
          <tmpfs>/approved-plan.json, and build with exactly those bytes:
            docker buildx build
              --build-arg BUILDKIT_SYNTAX=ghcr.io/railwayapp/railpack-frontend:v0.37.0
              -f <tmpfs>/approved-plan.json
              --build-arg cache-key=<job id>
              --output type=docker,dest=-        (streamed, §5.6)
              <tmpfs>/src
          (no --secret flags: build-time secrets are off, §4.6)

8. API    receive the artifact (§5.6), verify digests, load onto the target,
         start the container, record provenance
```

**Why the API may not be the one that prepares.** Two independent reasons, and
the second is decisive:

1. *Efficiency*: the API would have to download every repository — defeating
   the purpose of the worker.
2. *Correctness*: the plan is **repository-controlled** [M: F9]. A plan
   produced on the control plane and shipped to the worker is a plan the
   control plane has already trusted, which is exactly the thing to avoid. If
   the API cannot prepare safely on its own host, it must not prepare at all.

The API's remaining role is **control over the build's inputs** (images,
secrets, structure), not control over the build's content. This is stated
plainly because it is the single most important correction in this revision.

**Consequence.** `POST /api/source/analyze` and `POST /api/projects/from-source`
no longer call `source.Prepare` on the API host [C: `api/handlers/source.go:26`,
`api/handlers/project_sources.go:59`]. They enqueue a `prepare` job and wait
for it (a bounded synchronous wait; see §5.9 and open question Q5). The
existing per-user rate limit (6/min) and concurrency limit (2) move with the
job, and must remain enforced (they are currently in-process [C:
`api/source/limits.go:42-69`]; with a worker they have to be enforced at the
API, which is the only place that sees all users).

### 4.2 Worker supervisor and BuildKit privilege boundaries

**Problem.** Revision 2 specified a per-job BuildKit builder container that is
"not privileged", with `--cap-drop ALL`, `--security-opt no-new-privileges`,
a read-only rootfs, pids/memory limits and no host mounts.

**Measured reality** [M: F1]: `docker buildx create --driver docker-container`
produces a builder container with `Privileged=true`, `CapAdd=[]`, `CapDrop=[]`,
`SecurityOpt=["label=disable"]`, `NetworkMode=bridge`, `IpcMode=private`,
`Memory=0`, no pids limit, a writable rootfs, and exactly one mount (its own
BuildKit state volume). **No Docker socket is mounted into the builder.** And
`buildx create --help` exposes no cap-drop, no-new-privileges, read-only,
pids-limit, memory or user option — only `--driver-opt`,
`--buildkitd-config`, `--buildkitd-flags`, `--platform`, `--node`,
`--append`/`--leave`, `--bootstrap` and `--timeout`.

So Revision 2's builder hardening is **withdrawn as stated**. Three
configurations remain, and the RFC recommends one:

| Option | Isolation properties | Status |
| --- | --- | --- |
| **W1 — native rootless `buildkitd` on the worker (recommended)** | No Docker daemon on the host at all ⇒ no socket, no rootful daemon to talk to. `buildkitd` runs as a non-root user; build steps run under its own user namespace. | Partially measured [M: F3]: `moby/buildkit:rootless` starts **unprivileged** (`User=1000:1000`, `Privileged=false`) with `--security-opt seccomp=unconfined --security-opt apparmor=unconfined`; daemon reports `auto snapshotter: using overlayfs`, `process-mode: sandbox`, `platforms linux/amd64`, and warns "TLS is not enabled … mutual TLS recommended". The `RUN` step **failed on this machine** with `runc run failed: error mounting "proc" to rootfs at "/proc": operation not permitted` — a nested-container artefact of the measurement box, **not** evidence about a real VM. **Release blocker FT-2.** |
| **W2 — stock `docker-container` driver on a dedicated worker** | Builder container is `Privileged=true` [M: F1]. The host's **rootful** Docker daemon is reachable by any process running as the worker user ⇒ a build escape means root on the worker host. | Measured [M: F1, F2]. Acceptable **only** on a disposable, credential-free, network-isolated worker host whose compromise is assumed in the threat model. |
| **W3 — stock `docker` driver (host daemon)** | No isolation at all between build steps and the host daemon. | **Rejected.** |

**Recommended: W1**, with W2 as the documented fallback if W1 cannot be made to
work on the operator's OS (the fallback is only acceptable together with
§4.9's "assume the worker host is compromised" posture).

**Supervisor design [D].**

```
systemd unit: stagezero-build-worker
  User=stagezero-worker (uid ≥ 1000, no shell, no supplementary groups)
  AmbientCapabilities=            (empty)
  CapabilityBoundingSet=          (empty)
  NoNewPrivileges=yes
  ProtectSystem=strict, ProtectHome=yes, PrivateTmp=yes
  ReadWritePaths=/var/lib/stagezero-worker
  RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
  SystemCallFilter=@system-service ~@privileged ~@resources
  MemoryMax=8G, TasksMax=512, CPUQuota=400%
```

- The agent runs as a non-root user with **no Docker client installed at all**
  under W1.
- **Per job**, the agent starts its **own `buildkitd` process** on its own unix
  socket (`/run/stagezero-worker/job-<id>/buildkitd.sock`, mode 0600), with its
  own state directory under the job's tmpfs, and its own `--oci-worker-net`
  setting. One `buildkitd` per job gives: no cache survives a job (kills T8),
  no state survives a crash, and cleanup is "kill the process group".
  F3 measured that the default worker network mode is `host` — i.e. build steps
  share the daemon's network namespace. Whether `buildkitd --oci-worker-net=bridge`
  yields a separate per-step netns under rootless is **untested** (FT-3).
- **Job cancellation** kills the per-job `buildkitd` process group; the agent
  then wipes the job's tmpfs.
- **Boot janitor** (runs before the agent accepts work): kills leftover
  `buildkitd` processes recorded in `/run/stagezero-worker/*.pid`, removes
  `/var/lib/stagezero-worker/jobs/*`, and (under W2) removes builders labelled
  `stagezero.worker=<worker id>`.
- **Idle shutdown**: after N idle minutes the agent stops its builders so the
  host can be snapshotted; the systemd unit restarts it on demand.

**Container security options [D].** Build-step hardening is BuildKit's own
sandbox (runc), not Docker's. The worker therefore relies on: user namespaces
(W1), the default seccomp profile, a read-only worker filesystem, a tmpfs job
workdir, no host mounts, and the host firewall. Docker's `--cap-drop` family of
options is **not** available for the builder under W2 [M: F1] and is not
required under W1.

**Cleanup strategy [D].** Per job: (1) kill the job's `buildkitd` process
group; (2) unmount and delete the job tmpfs; (3) delete the job's socket
directory; (4) delete any partial artifact. Per worker boot: janitor as above.
Under W2: `docker buildx rm <builder>` and `docker rm -f` for the builder
container, keyed by the `stagezero.worker` label.

### 4.3 Gateway: shared process or isolated service?

**Resolution — the gateway is a separate process, a separate listener, a
separate credential domain, and a separate code path.**

The contradiction named in the request ("a narrowly scoped worker gateway
token" versus "no privileged control-plane credentials") is resolved by
separating **authentication** from **authority**:

- The worker presents a **worker token** to the gateway. The token is a
  *capability*, not an identity: it grants exactly six operations on exactly
  the jobs it holds, and it grants **no** data, no secret, no enumeration and
  no administrative action (§5.2).
- The worker holds **no** control-plane credential of any kind: not the
  database DSN, not `ENCRYPTION_KEY`, not an OAuth secret, not an SSH key, not
  a session token. There is nothing for it to escalate to.
- The gateway process holds **no** control-plane credential either: it has no
  database DSN and no master key. It authenticates workers, validates and
  forwards messages, and stores nothing. A gateway compromise therefore does
  **not** yield the master key, the SSH keys or the database.
- The gateway's assertions to the API are treated with exactly the trust level
  of the *worker* they carry, never as privileged assertions. The API must not
  accept "the gateway says this artifact is fine" as a stronger statement than
  "the worker says this artifact is fine".

**Consequences of a gateway compromise [D].** The attacker can impersonate a
worker to the API: submit a false plan, submit false events (log poisoning),
and upload an arbitrary artifact that the API may then deploy to a customer
target. The gateway cannot steal control-plane secrets, because it does not
hold them. The residual harm is therefore "the images we build", not "the
platform". §4.9 states what that means and what it does not.

**Why a separate process rather than a route in the API process.** A route in
the API process would share the API's memory space, its `ENCRYPTION_KEY`, its
database pool and its crash domain: a bug in the worker-facing handler would
be a control-plane compromise. A separate process gives: a separate crash
domain, a separate release cadence, a separate credential scope, and the
ability to place the gateway on a different interface or host later. The cost
is one more process to operate and one more hop.

**Transport [D].** The gateway listens on a dedicated port (default 8443) with
TLS. Worker authentication is a bearer token **plus** mTLS when the operator
can issue client certificates; token-only is the minimum. The gateway forwards
to the API over a unix socket on the API host.

### 4.4 Network policies per phase

Four distinct phases with **separate** policies. The worker is not "a host
with egress"; it is a host that changes posture per phase.

| Phase | Where | Permitted egress | Denied | Enforced by |
| --- | --- | --- | --- | --- |
| **P-A Acquisition** | worker agent | `codeload.github.com:443` (DNS to the worker's resolver only) | Everything else; all RFC1918/CGNAT; link-local; metadata | Host firewall (nftables) + resolver allow-list |
| **P-B Preparation** | per-job sandbox | **None** | All | Sandbox has no network namespace route [D]; verified feasible for 5 of 9 providers [M: F7] |
| **P-C Dependency install / build** | build steps | Per-job provider allow-list (§8) + the toolchain distribution hosts | Everything else | Filtering proxy on the worker, referenced by the build environment |
| **P-D Artifact upload** | worker agent | `gateway:8443` only | Everything else, including the control plane | Host firewall |

**Deny-list that applies to every phase [D].**

| Class | Addresses |
| --- | --- |
| IPv4 private | 10/8, 172.16/12, 192.168/16 |
| Carrier NAT | 100.64/10 |
| Link-local / metadata | 169.254/16 (incl. 169.254.169.254), 100.100.100.200 |
| Cloud metadata names | `metadata.google.internal`, `metadata.goog` (DNS: NXDOMAIN) |
| IPv6 | **all of it in v1** (dropped at the host firewall); `fd00:ec2::254` included |
| Control plane | the API host, the Postgres host, the gateway's other ports |
| Docker | 2375/2376, the unix socket |
| Proxies | the build environment's `HTTP_PROXY`/`HTTPS_PROXY`/`ALL_PROXY`/`NO_PROXY` are **overwritten** by the worker to point at the local filtering proxy; a repository cannot select its own egress path |

**Verified constraints on this design [M: F2].** Through the stock driver a
`RUN` step reached `https://example.com` — build steps have egress unless the
network removes it. A repository can also influence its own proxy configuration
through environment or config files, which is why the proxy variables are
overwritten rather than inherited.

**F8 lesson [M].** The allow-list must include the **toolchain distribution**
hosts (`mise.jdx.dev` and the per-toolchain download hosts), not only package
registries; a build that installs `bun 1.4.2` failed here purely because
`mise.jdx.dev` did not resolve.

**Egress separation caveat (honest limitation) [D, M: F3].** Under W1 the
build steps share the daemon's network namespace by default
(`org.mobyproject.buildkit.worker.network:host`), so the *image-pull* traffic
(registries) and the *dependency-install* traffic (packages) traverse the same
path and cannot be separated by policy alone. `buildkitd --oci-worker-net=bridge`
is the intended separation mechanism; whether it works under rootless is
**untested** (FT-3). In v1, one combined per-provider allow-list is used, and
the image references are digest-pinned by the API, which limits what a build
step can usefully pull.

### 4.5 Leases, termination, orphans, stale uploads, retries, idempotency

**Job state machine [D].**

```
                 ┌──────────┐
   enqueue ────► │  queued  │
                 └────┬─────┘
                      │ lease granted (fence++)
                 ┌────▼─────┐  lease lost / worker died
                 │  leased  │ ──────────────┐
                 └────┬─────┘               │
                      │ worker starts       │
                 ┌────▼─────┐               │ reaper: fence++ → requeue
                 │ running  │               │ (attempt+1, ≤ 3) else fail
                 └────┬─────┘               │  (worker_lost)
      ┌───────────────┼───────────────┐     │
      │               │               │     │
 ┌────▼────┐  ┌───────▼──────┐  ┌────▼────┐│
 │succeeded│  │   failed     │  │cancelled││
 └─────────┘  └──────────────┘  └─────────┘│
                                             │
        ┌────────────────────────────────────┘
        ▼
   ┌──────────┐
   │abandoned │  (terminal; operator-visible)
   └──────────┘
```

**Lease and fence [D].**

| Field | Value |
| --- | --- |
| Lease TTL | 120 s |
| Heartbeat interval | 30 s |
| Fence | monotonically increasing `BIGINT` per job, incremented on every lease grant |
| Stale write | any mutation (event, artifact, completion) whose `(lease_id, fence)` is not the current one is rejected with **409 stale_fence**, and the payload is discarded (for an artifact upload, the partially written file is deleted) |
| Fence in the artifact record | yes — the artifact row stores the fence that produced it, so a stale artifact cannot be attached to a re-queued job |

**Worker-side termination rules [D].**

- Three consecutive heartbeat failures, or a lease with less than 30 s remaining
  ⇒ the worker **must** terminate the build it is running (kill the job's
  `buildkitd` process group), wipe the job tmpfs, and stop. It must not
  continue a build it no longer owns, and it must not upload an artifact.
- A heartbeat response carrying `cancel: true` ⇒ the worker stops the solve
  within 10 s, uploads nothing, and reports `cancelled`.
- The API's hard deadline (prepare 90 s, build 30 min — the current values
  [C: `api/jobs/deploy.go:36-37`, `api/source/limits.go`]) marks the job
  terminal and increments the fence; the worker's later writes are then stale
  and rejected.

**Crash recovery [D].** An API reaper runs every 30 s. It finds jobs in
`leased`/`running` whose lease has expired, increments the fence, and either
re-queues (attempt + 1, up to 3 attempts) or fails the job with
`worker_lost`. Re-queueing is safe because each attempt gets a fresh fence, a
fresh sandbox and a fresh artifact slot.

**Orphan cleanup [D].**

| Orphan | Detection | Cleanup |
| --- | --- | --- |
| Worker process dies mid-job | Lease expiry + reaper | Job re-queued; the orphaned `buildkitd` on the worker is killed by the worker's own supervisor on restart and by the boot janitor |
| `buildkitd` survives its job | PID file under `/run/stagezero-worker` | Killed by the janitor on worker boot; killed by the agent after each job |
| Partial artifact on the worker | Job tmpfs wiped on job end and at boot | Deleted |
| Partial artifact on the API | Upload writes to a temp file; the file is renamed into place **only after** digest verification | Temp file deleted on rejection |
| Builder container under W2 | Label `stagezero.worker=<id>` | `docker buildx rm` / `docker rm -f` by the janitor |

**Retry safety [D].** A retry produces a possibly different image (a build
without a lockfile is not reproducible) — accepted and recorded: the artifact
digest of the successful attempt is what gets stored. Failed attempts leave no
artifact.

**Idempotent deployment transitions [D].** The deployment row is the unit of
idempotency, not the job. Requirements:

1. The deployment records `artifact_id`, `artifact_sha256` and `image_id`
   (the image ID observed **on the target** after `docker load`). A resumed
   deployment that already has a verified artifact **does not rebuild**.
2. The target-side sequence is idempotent: `docker load` is skipped when the
   target already holds an image with the same ID; `docker run` uses the
   deterministic container name and the existing reconciler
   [C: `api/jobs/deployment_reconciler.go`] performs the stop/replace.
3. Every terminal transition is written exactly once, guarded by the
   deployment's current status; a late worker message cannot flip a terminal
   deployment back to `in_progress`.
4. The fencing token is carried from the job to the deployment, so a stale
   worker cannot complete a deployment that a newer attempt already owns.

### 4.6 Build-time secrets disabled by default

**Resolution.** Build-time secrets are **off**, and the mechanism is removed
rather than flagged:

- The job envelope contains no secret field at all.
- The API empties the plan's `secrets` list and rewrites every step's
  `secrets` list to `[]` before approval (the plan carries names only
  [M: F6]; the values never live in the plan).
- The worker never passes `--secret` to BuildKit.
- If a service's configuration requires a build-time secret, the build is
  **refused** with `build_secret_required` and the message names the service
  and the variable. The refusal is explicit; it is not a silent build failure
  inside the sandbox.

**Consequence, stated plainly.** Today, service env vars are passed to both
the analysis and the build [C: `api/jobs/deploy.go:431-433`]. After this
change they are passed to the **runtime container only**. Repositories that
need a token at build time (private registries, `NPM_TOKEN`, `CARGO_REGISTRY_TOKEN`,
private Python indexes) **will stop building**. This is a deliberate,
product-visible reduction of capability, and it is one of the recommended
operator decisions (§14, Q6).

**Future, explicitly authorised mechanism [D — not in this phase].** If
operators later require build-time secrets, the minimum acceptable design is:
a **separate encrypted store** (not the global `ENCRYPTION_KEY`) scoped to one
service and one workspace; a per-service, per-workspace opt-in flag with an
explicit "this repository is untrusted" acknowledgement; delivery only as a
BuildKit secret mount sourced from a tmpfs file on the worker, never as an
environment variable and never through the gateway; a TTL; and a rule that the
value is redacted from every log surface. That mechanism is **out of scope**
until separately authorised.

**Private registries.** Deferred (§14, Q7): authenticating to a private
registry requires a credential in the build environment, which is exactly the
thing this phase removes.

### 4.7 Corrected AT-2 — a malicious build printing a secret

The original AT-2 was invalid: it asked the worker to print a secret that, in
the corrected design, the worker does not have. The corrected test must
therefore be built on the one secret class that can still reach a build: the
**source repository's own committed content**, and the **runtime env values**
that the API holds but does not give to the build.

**Corrected AT-2 [D].** *A build step prints a string equal to a runtime env
secret of its own service (obtained by any means, e.g. read from a file the
repository committed, or guessed). The value must not appear in worker events,
the API's own logs, `deployment_logs`, the artifact's metadata, or the
diagnostics bundle.*

**How it is satisfied, in layers:**

| Layer | Mechanism |
| --- | --- |
| Structural (primary) | The build environment contains no control-plane or runtime secret: build-time secrets are off (§4.6) and the worker's environment allow-list is the same one already used for Railpack [C: `api/source/source.go:672-679]`. A build *cannot* print a value it was never given. |
| Control-plane redaction (defence in depth) | The API maintains a **redaction set** = the decrypted runtime env values of the services in the deployment's workspace. Every worker event is passed through the redactor **before it is stored, streamed to the browser, or written to the API's own log** — the same technique already used for build output [C: `api/jobs/deploy.go:828-845`], applied at the API boundary rather than only at the log sink. |
| Event sanitisation | Worker events are length-capped (8 KiB/line), stripped of ANSI escapes and control characters, and never interpolated into a log format string; they are stored as parameters, not as format arguments. |
| Diagnostics | The diagnostics bundle is assembled from the already-redacted store; the raw worker stream is never uploaded. |
| Artifact | The artifact is treated as **opaque**: it is never extracted, indexed or searched by the API. (An image that contains a secret the repository itself committed is the repository owner's content — see §4.9.) |

**Residual, stated explicitly.** A build *can* print a secret that the
repository itself committed, and the image can contain it. That is the
repository owner's content, not a control-plane secret, and the RFC does not
claim to prevent it (§4.9, T9).

### 4.8 The precise API ↔ worker trust boundary

**The boundary is the set of messages that cross the gateway, and nothing
else.**

```
   WORKER (untrusted)            GATEWAY (minimal)              API (trusted)
 ─────────────────────           ────────────────              ─────────────
 job request   ──────────────►   validate token+scope ────────►  enqueue job
 plan          ──────────────►   size/shape checks ──────────►  validate plan
                                (no plan logic lives here)      pin images
 events        ──────────────►   line framing, size cap ──────►  redact, store
 artifact      ──────────────►   streaming, length cap ──────►  hash, store
 heartbeat     ──────────────►   lease bookkeeping ──────────►  fence check
 completion    ──────────────►   fence check ────────────────►  state machine
```

**Rules that make the boundary real [D].**

1. The worker authenticates **only** to the gateway, with a worker token. The
   API's normal session-cookie authentication is **not** accepted on the
   gateway, and the worker token is **not** accepted on listener A.
2. The gateway routes are a **fixed set of six**. There is no list, no get, no
   admin, no user, no workspace, no secret, no admin-surface route.
3. Every message carries the job id, the lease id and the fence. The gateway
   does not interpret business meaning; the API does.
4. The gateway holds no credential that can read tenant data.
5. The worker token is scoped to a set of workspaces, job kinds and
   architectures (§5.2), and every request is checked against that scope.
6. The API is the only component that can terminate a deployment, and it
   accepts a worker's completion only with a valid fence on a live lease.

**The contradiction, resolved in one sentence:** the worker token is a
*capability scoped to one job family*, and because it is a capability rather
than an identity, granting it does not grant the holder any control-plane
credential, so "narrowly scoped token" and "no privileged control-plane
credentials" are simultaneously true.

### 4.9 What a compromised worker can and cannot do

**Can [D].**

| Capability | Consequence |
| --- | --- |
| Read the source it was assigned | The source is public or already known to the workspace |
| Return a malicious image for a legitimate customer | **Supply-chain compromise of that customer's application** |
| Claim a different source SHA, plan or digest | Lying to the API |
| Stall, loop, or flood the job queue | Denial of service on source deployments |
| Exhaust its own disk/CPU | Denial of service on that worker only |

**Cannot [D].** Reach the control plane, the database, the master key, any
workspace's SSH key, any service's env secret, any session, or any customer
target. The worker's only route out is the gateway, and the gateway gives it
nothing but the next job's inputs. **That is the meaningful boundary: a
compromised worker is a compromised *builder*, not a compromised *platform*.**

**What the API's checks do and do not prove [D].**

| Check | Proves | Does **not** prove |
| --- | --- | --- |
| Artifact SHA-256 matches the received stream | The bytes are intact and identical to what the worker produced | That the worker was honest, or that the image matches the source |
| The target's loaded image ID equals the reported image ID | The target holds exactly the artifact's bytes | That the artifact is safe to run |
| Every image in the plan is digest-pinned and resolved by the API | The build's *inputs* are fixed and reproducible | That the *builder* was honest |
| The reported source SHA equals the API's independently resolved SHA | The worker *claimed* the same commit the API resolved | That the worker built that commit |
| Provenance record (job, fence, plan digest, source SHA, digests, worker id, timestamps) | An audit trail for incident response | Any cryptographic proof of origin; it is unsigned metadata |

**Required statement for the release notes.** Checksums, image pinning and
provenance metadata **do not** establish the honesty of an untrusted worker.
The RFC makes no such claim. A worker compromise must be handled as
"**assume every image produced by that worker since its last verified job is
tainted**": revoke the worker token, re-queue the affected deployments onto a
fresh worker, and notify the affected customers. This is an operational
procedure (Phase 2 item), not a technical guarantee.

**The residual, honestly stated.** For the *customer's own repository*, the
application code is untrusted by design; the marginal harm of a compromised
worker is that the customer's image may not match their repository. For a
workspace that deploys **third-party** repositories, the worker is a genuine
supply-chain risk, and only attestation by a trusted builder (Phase 2/3) or
in-toto-style verification would reduce it. That reduction is explicitly
**not** delivered by this RFC.

### 4.10 Untrusted application runtime containers on customer targets

**Problem.** The deployed application is untrusted code that runs on the
customer's server with the customer's Docker daemon [C:
`api/jobs/deploy.go:699-746`], on the shared `stagezero` network
[C: `api/proxy/setup.go:37`], with the customer host's Docker socket reachable
through the monitoring agent's mounts [C: `api/monitoring/agent.go:216`].
SZ-12 removes the *build* from the customer host; it does not make the
*runtime* safe.

**Resolution — one surgical in-scope change, everything else explicitly
deferred to SZ-13.**

In scope for SZ-12 (Phase 0):

1. **Move the monitoring agent to its own Docker network.** Measured: an
   application container on the shared `stagezero` network reaches the agent's
   container IP **successfully even though the agent's port is published on
   `127.0.0.1`** [M: F5, cases A and H3]; traffic between two user-defined
   networks is **dropped** [M: F5, cases D and H2]; a loopback-published port
   is unreachable from **any** container [M: F5, cases B, C, E1, E2, H1, H4,
   H5]; and the host reaches `127.0.0.1:<agentPort>` successfully [M: F5,
   case F], so the API's SSH tunnel to the agent keeps working unchanged. The
   effective control is the **network**, not the publish binding. Residual,
   stated honestly: the **host** always has a path to the agent's port [M:
   F5, case G], and the agent still holds `docker.sock` and `/` read-only
   mounts [C: `api/monitoring/agent.go:216] ⇒ the agent is still a
   host-read/host-control surface for a compromised application container.
2. **Move runtime env values off the target's argv** (T4): use `--env-file`
   with a 0600 file instead of `--env 'K=V'` [C: `api/jobs/deploy.go:741`].
   This is a small, self-contained change with a direct confidentiality
   benefit.

Explicitly **out of scope** (SZ-13, tracked separately, not silently absorbed
here): capability dropping and `no-new-privileges` for application
containers; pid/memory/cpu limits; read-only root filesystems; rootless
containers; seccomp/AppArmor profiles; per-service egress policy; image
scanning; the monitoring agent's `docker.sock` and `/` mounts; and
`deploy.startCommand`, which is repository-controlled [M: F9] and therefore
attacker-chosen by construction.

## 5. Protocol and lifecycle specification

### 5.1 Data model (new tables, documentation only)

**`build_jobs`**

| Column | Type | Notes |
| --- | --- | --- |
| `id` | TEXT PK | `job-` + uuid |
| `workspace_id` | TEXT NOT NULL | scope check for the worker token |
| `service_id` | TEXT NULL | NULL for `prepare` jobs |
| `kind` | TEXT NOT NULL | `prepare` \| `build` |
| `state` | TEXT NOT NULL | `queued` \| `leased` \| `running` \| `succeeded` \| `failed` \| `cancelled` \| `abandoned` |
| `attempt` | INTEGER NOT NULL | 1-based |
| `max_attempts` | INTEGER NOT NULL | 3 |
| `fence` | BIGINT NOT NULL | incremented on every lease grant |
| `lease_id` | UUID NULL | |
| `lease_owner` | TEXT NULL | worker id |
| `lease_expires_at` | TIMESTAMPTZ NULL | |
| `cancel_requested_at` | TIMESTAMPTZ NULL | |
| `deadline_at` | TIMESTAMPTZ NULL | |
| `source_owner`, `source_repo`, `source_branch`, `source_sha` | TEXT | resolved coordinates |
| `source_tarball_sha256` | TEXT NULL | filled by the worker after acquisition |
| `plan_sha256` | TEXT NULL | the **approved** plan digest |
| `plan_json` | TEXT NULL | the approved canonical plan |
| `artifact_id` | TEXT NULL | |
| `artifact_sha256` | TEXT NULL | |
| `image_id` | TEXT NULL | observed on the target |
| `error_code` | TEXT NULL | `worker_lost`, `deadline_exceeded`, `stale_fence`, `build_secret_required`, `unsupported_platform`, … |
| `created_at`, `updated_at`, `started_at`, `finished_at` | TIMESTAMPTZ | |

**`worker_tokens`**

| Column | Type | Notes |
| --- | --- | --- |
| `id` | TEXT PK | |
| `worker_id` | TEXT NOT NULL | |
| `token_hash` | TEXT NOT NULL | Argon2id or SHA-256 with a per-token salt; the token itself is shown once |
| `allowed_workspaces` | TEXT[] | empty = all workspaces the operator allows |
| `allowed_job_kinds` | TEXT[] | `prepare`, `build` |
| `allowed_arch` | TEXT[] | e.g. `amd64` |
| `status` | TEXT NOT NULL | `active` \| `revoked` \| `superseded` |
| `expires_at` | TIMESTAMPTZ NULL | |
| `revoked_at` | TIMESTAMPTZ NULL | |
| `superseded_by` | TEXT NULL | rotation linkage |

### 5.2 Gateway API surface (exactly six endpoints)

| # | Method + path | Auth | Scope check | Limit |
| --- | --- | --- | --- | --- |
| 1 | `POST /v1/jobs/claim` | worker token | job kind, workspace, arch | returns the job, its fence, its lease, and the source coordinates |
| 2 | `POST /v1/jobs/{id}/heartbeat` | token + lease + fence | job must be leased by this worker | returns `{fence, lease_expires_at, cancel}` |
| 3 | `POST /v1/jobs/{id}/plan` | token + lease + fence | kind = `prepare` | ≤ 1 MiB JSON; the API validates and returns the approved plan |
| 4 | `POST /v1/jobs/{id}/events` | token + lease + fence | kind = `build` | ≤ 8 KiB per line, ≤ 2000 lines/minute |
| 5 | `POST /v1/jobs/{id}/artifact` | token + lease + fence | kind = `build` | streaming; `Content-Length` required; ≤ 2 GiB |
| 6 | `POST /v1/jobs/{id}/complete` | token + lease + fence | kind = `build` | `{status, image_id, error_code}` |

No other route exists on the gateway. In particular there is **no** route that
lists jobs, returns another job's data, returns a secret, or mutates a
deployment.

### 5.3 Plan validation rules (the API's control-object check)

Derived from the 9-provider corpus [M: F6] and the repository-controlled-plan
finding [M: F9]. The rule is a **structural allowlist**, not a blocklist: any
key not observed in the corpus is a rejection.

| Rule | Action |
| --- | --- |
| Top-level keys ∈ {`$schema`, `caches`, `deploy`, `secrets`, `steps`} | reject otherwise |
| `deploy` keys ∈ {`base`, `inputs`, `startCommand`, `variables`} | reject otherwise |
| `steps[]` keys ∈ {`name`, `assets`, `commands`, `inputs`, `caches`, `secrets`, `variables`} | reject otherwise |
| `commands[]` keys ∈ {`cmd`, `customName`, `dest`, `image`, `mode`, `name`, `path`, `src`} | reject otherwise |
| `inputs[]` keys ∈ {`include`, `exclude`, `image`, `local`, `step`} | reject otherwise |
| `caches{}.type` ∈ {`shared`, `locked`} | reject otherwise |
| Any image reference | resolved by the API to a digest and rewritten; a reference outside the image policy is rejected |
| Any secret list | emptied (build-time secrets off) |
| Any key or value that looks like a mount, socket, privileged, capability, or host-path directive | rejected (none occur today [M: F6]; the rule is there for a future Railpack version) |
| Total size | ≤ 1 MiB |
| `steps[].commands[].cmd` | **not** filtered — commands are repository-controlled [M: F9] and filtering them would be security theatre; the sandbox is the boundary |
| Reported railpack version | must equal 0.37.0 |
| Reported source SHA | must equal the SHA the API resolved |

**CI gate.** The 9-provider corpus is pinned in the repository, and CI
re-derives the key union from the pinned Railpack binary on every version
bump; a change in the key union fails CI until the allowlist is updated
explicitly. This is the mechanism that keeps the allowlist honest across
Railpack upgrades.

### 5.4 Artifact transfer protocol

| Aspect | Rule |
| --- | --- |
| Producer | The worker, streaming `docker buildx build --output type=docker,dest=-` into the upload. The worker never stores a 2 GiB artifact on disk. |
| Direction | Worker → gateway → API. The worker never connects to a customer target. |
| Framing | HTTP chunked upload with a required `Content-Length`; the API rejects a missing length and aborts above 2 GiB |
| Streaming | The API streams to a temp file on the artifact volume while computing SHA-256; memory use is bounded |
| Integrity | The API compares the computed digest with the digest the worker reported; mismatch ⇒ delete the temp file, fail the job with `artifact_digest_mismatch` |
| Placement | The file is renamed into place **only after** verification, so a partial file is never visible as an artifact |
| Fence check | Performed **before** the write and again before the rename; a stale fence aborts and deletes |
| Download | The API streams the artifact to the customer target over the existing SSH channel and pipes it into `docker load` |
| On-target verification | `docker image inspect --format '{{.Id}}' <image>` must equal the worker's reported `image_id`; otherwise the deployment fails with `target_image_mismatch` and the image is not started |
| Retention | 24 h TTL, then deleted; per-workspace quota; deleted immediately on cancellation or failure |
| Encryption at rest | The artifact volume is encrypted (disk or volume-level); the artifact is a container image, not a secret store, but it contains the customer's source |
| Provenance | Stored alongside: job id, fence, plan digest, source SHA, source tarball digest, resolved image digests, worker id, railpack version, timestamps. **Unsigned metadata — an audit trail, not a proof** (§4.9) |
| Cleanup | Deleted on job failure, on cancellation, on TTL expiry, and on the deployment's retention policy |

### 5.5 Credential handling

| Credential | Where it lives | How the worker sees it |
| --- | --- | --- |
| `ENCRYPTION_KEY` | API process memory only | Never |
| `DATABASE_URL` | API process | Never |
| OAuth client secrets | API process | Never |
| Workspace SSH private keys | Encrypted at rest, decrypted in the API | Never |
| Service env var values | Encrypted at rest, decrypted in the API | Never (build-time: off; runtime: shipped to the target container only) |
| Session tokens | Database (plaintext) | Never |
| Monitoring agent tokens | Encrypted at rest | Never |
| Worker token | Issued once to the operator, stored hashed | The worker holds **only** this one credential |

The worker's environment for any child process is the same allow-list already
used for Railpack [C: `api/source/source.go:672-679]: `PATH`, `HOME`, `TMPDIR`,
`LANG`, `LC_ALL`, `SSL_CERT_FILE`, `SSL_CERT_DIR`, `XDG_CACHE_HOME` — plus the
build's own pinned proxy variables, which the worker **overwrites**.

### 5.6 Worker lifecycle (per job)

```
claim ──► prepare (kind=prepare)            ┐
        └─► POST /plan ──► approved plan     │
        └─► build (kind=build)                │
             └─► POST /events (streamed)      │
             └─► POST /artifact (streamed)    │
             └─► POST /complete               ┘
```

1. **Claim.** The worker claims a job; the API assigns `lease_id`, `fence` and
   `lease_expires_at`.
2. **Acquire.** For a `build` job the worker needs the source: either the API
   relays the tarball (the API already downloads it today) or the worker
   fetches it itself under policy P-A. **Recommendation [D]: the API relays
   the tarball.** Rationale: the API must independently know the source
   (it resolves the SHA and can hash the tarball itself), the worker's egress
   shrinks to the gateway only, and the acquisition policy P-A disappears
   entirely. The worker verifies the relayed tarball's SHA-256 against the
   digest the API supplies and refuses a mismatch.
3. **Prepare.** Extraction into the job's tmpfs with the existing caps
   (500 MiB tarball, 512 MiB extracted, 20 000 entries, traversal and symlink
   containment) [C: `api/source/source.go:457-628`]; then `railpack prepare`
   inside the sandbox with the environment allow-list.
4. **Plan.** The candidate plan goes to the API; the API validates,
   canonicalises, digest-pins and returns the approved plan; the worker
   verifies the approved plan's digest and uses exactly those bytes.
5. **Build.** `docker buildx build` with the pinned frontend constant
   [C: `api/jobs/deploy.go:33`], `-f approved-plan.json`, `--build-arg
   cache-key=<job id>`, `--output type=docker,dest=-`, context = the job's
   extracted source. Under W1 the `buildctl` equivalent is used; the frontend
   is selected the same way (`BUILDKIT_SYNTAX`).
6. **Events.** Build output is streamed to the gateway, redacted and
   size-capped by the API before storage.
7. **Artifact.** Streamed to the API; digests compared; the file is placed
   only after verification.
8. **Complete.** The worker reports the outcome and the image id.
9. **Cleanup.** Kill the job's `buildkitd`; wipe the tmpfs; delete the socket
   directory; delete any partial artifact.

### 5.7 Observability

| Signal | Where |
| --- | --- |
| Job state transitions | `build_jobs` (auditable, with the fence) |
| Build output | `deployment_logs` (existing table [C: `api/db/migrations/0001`, `0015`, `0017`]), redacted before storage |
| Worker health | Heartbeat timestamps, per-worker job counters, last error |
| Queue depth and wait time | `build_jobs` counts by state; the API's 429 behaviour is unchanged |
| Egress denials | The filtering proxy's log, shipped to the API as an event (no payload) |
| Failed builds | Error code, phase, and the redacted tail |
| Security events | `stale_fence` rejections, `artifact_digest_mismatch`, `target_image_mismatch`, plan rejections — all counted and alerted |

### 5.8 Egress policy summary (see §4.4 for the tables)

The worker host's firewall is the authority. The API's per-provider allow-list
may only **narrow** it; a mismatch between the API's expectation and the
worker's actual reachability **fails the job closed** rather than proceeding
on the assumption that the firewall holds.

## 6. Security invariants

Each invariant names its enforcement mechanism and the test that proves it.
"Untested" means the guarantee is a design intent until the named test passes.

| ID | Invariant | Enforcement | Test |
| --- | --- | --- | --- |
| I1 | Untrusted content is never parsed on the control plane | The control plane has no repository-fetch or Railpack code path; `source.Prepare` is unreachable from the API handlers | AT-1 |
| I2 | Untrusted content never executes on a customer target | The target receives an image; the target-side `docker buildx build` path is deleted in Phase 0 | AT-1 |
| I3 | The worker holds no control-plane credential | The worker's process environment is the allow-list; no config file, no env file, no key | AT-3 |
| I4 | The worker can reach only the gateway | Host firewall deny-by-default; per-phase policies | AT-4, AT-5, FT-4, FT-5 |
| I5 | The worker cannot reach the metadata service | Deny-list for 169.254.169.254, fd00:ec2::254, 100.100.100.200, metadata names | AT-5 |
| I6 | The worker cannot choose the images in a build | The API resolves and rewrites every image reference to a digest | AT-7 |
| I7 | The approved plan is the plan that is built | The worker verifies the approved plan's digest; the API stores the exact bytes | AT-7 |
| I8 | No build runs with a secret in its environment | The job envelope has no secret field; the plan's secret lists are emptied; no `--secret` flag | AT-6, FT-9 |
| I9 | A stale worker cannot write anything | Fence check on every mutation; stale artifact uploads are deleted | AT-10, FT-6 |
| I10 | A job runs at most once per attempt and completes exactly once | Fence + lease + `uq_deployments_in_progress_service` [C: `api/db/migrations/0024`] | AT-10, FT-6 |
| I11 | A build cannot outlive its lease | Worker self-termination on heartbeat failure; the API's hard deadline | FT-6, FT-7 |
| I12 | No artifact is deployed unless its digest matches on both ends | API-side digest, plus `docker image inspect` on the target | AT-13, FT-8 |
| I13 | A failed job leaves no artifact and no orphan | Per-job cleanup, boot janitor, TTL | FT-7, FT-8 |
| I14 | Build output cannot inject into the API's logs or the database | Redaction, sanitisation, parameterised storage, opaque artifacts | AT-2, AT-12, FT-9 |
| I15 | An application container cannot reach the monitoring agent | Separate Docker networks | AT-14, F5 |
| I16 | The worker cannot mint or escalate to a worker token | The API is the only issuer; the gateway verifies against a hash | AT-3 |
| I17 | The gateway holds no tenant secret | Separate process, no DB credential, no master key | Code review + configuration test |
| I18 | The plan allow-list cannot drift silently | CI re-derives the key union from the pinned corpus on every Railpack bump | AT-16, CI gate |
| I19 | The worker host is disposable and contains no data | Operator procedure: the host is rebuilt from an image; the API stores all state | Operator runbook |
| I20 | A worker compromise is contained to the images it built | Egress deny-list + no credentials + gateway scope (§4.9) | AT-4, AT-3 |

## 7. Verified versus unverified guarantees

### 7.1 Verified today (measured)

| ID | Guarantee | Evidence |
| --- | --- | --- |
| V1 | The stock `docker buildx` docker-container driver creates a **privileged** builder; `buildx create` cannot harden it | F1 |
| V2 | The rootless BuildKit image starts **unprivileged** as uid 1000; build-step execution on this nested machine failed with a `proc` mount error | F3 |
| V3 | Build steps have network egress through the stock driver | F2 |
| V4 | `docker buildx build` has **no** `--frontend` flag; the Railpack frontend is selected with `BUILDKIT_SYNTAX` | F4 |
| V5 | Same-network containers reach each other's container ports **even when published on `127.0.0.1`**; cross-user-defined-network traffic is **dropped**; loopback-published ports are unreachable from any container; the host always has a path to container ports | F5 (13 cases) |
| V6 | The Railpack plan's key space is knowable across 9 providers; `mount`/`privileged`/`network`/`entitlement`/`/var/run`/`ssh`/`--cap` occur **zero** times | F6 |
| V7 | `railpack prepare` completes with egress blocked for node, python, go, ruby and java, and runs no repository lifecycle scripts | F7 |
| V8 | A build needs the **toolchain distribution** hosts, not only package registries | F8 |
| V9 | Repository content controls the plan's commands, start command, toolchain, package manager, base image and caches | F9 |
| V10 | The 9-provider corpus prepares successfully with the pinned 0.37.0 binary | F6 |

### 7.2 Design intent, not yet verified

| ID | Guarantee | Gate |
| --- | --- | --- |
| U1 | Rootless BuildKit executes real Railpack build steps | FT-2 (release blocker) |
| U2 | A hostile build step cannot reach the host runtime or the control plane | FT-3 (release blocker) |
| U3 | Deny-by-default egress admits only the allow-list and refuses everything else | FT-4 (release blocker) |
| U4 | Per-step network separation from the daemon's registry egress | FT-3 / Phase 2 |
| U5 | Lease, fence, cancellation and crash recovery behave as specified | FT-6, FT-7 |
| U6 | Artifact corruption, truncation and replay are detected | FT-8 |
| U7 | The corrected AT-2 holds across every log and artifact surface | FT-9 |
| U8 | All 9 providers build end to end through the worker | FT-10 |
| U9 | The artifact size and the worker's peak consumption fit the proposed limits | FT-11 |
| U10 | `--output type=docker,dest=-` streams correctly to the upload | FT-11 |

## 8. Compatibility and regression matrix (Railpack 0.37.0)

| Provider | Detected as | Prepare offline [M: F7] | Images in the plan | Build-time egress needed | Notes and known limits |
| --- | --- | --- | --- | --- | --- |
| Node (bun) | `node` | **verified** | `railpack-builder` + `railpack-runtime` (`ghcr.io/railwayapp`, tag `mise-2026.8.6`) | `mise.jdx.dev`, toolchain download hosts, bun/npm registry | `bun install --frozen-lockfile` **requires a lockfile** — repositories without one fail; the railpack.json case showed the package manager can be switched to `npm` [M: F9] |
| Python | `python` | **verified** | same two | PyPI, `mise.jdx.dev` | `pip install -r requirements.txt`; `python -m venv /app/.venv` |
| Go | `golang` | **verified** | same two | `proxy.golang.org`, `sum.golang.org`, `mise.jdx.dev` | `go mod download`; `go build -ldflags="-w -s" -o out` |
| Ruby | `ruby` | **verified** | same two | `rubygems.org`, `mise.jdx.dev` | cache type **`locked`** — a shared cache would serialise or corrupt concurrent builds ⇒ per-job builder is mandatory, not optional |
| PHP | `php` | not re-verified offline | **`composer:latest`**, **`dunglas/frankenphp:php8.2.34-trixie`** (third-party, mutable tags) | packagist, `mise.jdx.dev`, apt repositories | the two third-party images are **mutable tags** and must be digest-pinned by the API; `composer install --no-scripts` is railpack's own hardening |
| Rust | `rust` | not re-verified offline | same two | `crates.io`, `static.crates.io`, apt repositories, `mise.jdx.dev` | `cargo build --release`; `apt-get install` inside the build |
| Java | `java` | **verified** (Maven 3.10.0 resolved) | same two | Maven Central, apt repositories, `mise.jdx.dev` | `mvn … clean dependency:…`; cache `.m2/repository` is a **relative** path |
| .NET | `Dotnet` | not re-verified offline | same two | `nuget.org`, `mise.jdx.dev` | cache type **`locked`**; `dotnet restore` / `dotnet publish` |
| Elixir | `Elixir` | not re-verified offline | same two | `hex.pm`, `mise.jdx.dev` | `mix local.hex --force`, `mix deps.get --only prod`, `mix release` |

**Cross-cutting compatibility rules.**

| Rule | Reason |
| --- | --- |
| The frontend pin stays `ghcr.io/railwayapp/railpack-frontend:v0.37.0` [C: `api/jobs/deploy.go:33`] | Preserves the existing build contract; the worker uses the same constant |
| The plan's `deploy.inputs` copies `.` into the runtime image | Repository content reaches the runtime image by construction (T9) — accepted and documented, not fixed here |
| The `locked` cache type in Ruby and .NET | Requires per-job builders (already the design) |
| Toolchain versions are resolved by Railpack and constrained by its own generated mise config (`minimum_release_age = "14d"`, `paranoid = true`, `trusted_config_paths = ["/app"]`) [M: F6] | Railpack's supply-chain hardening is preserved; the API pins the *images*, and the *toolchain version* is a railpack decision the API may additionally constrain (§14, Q9) |
| `railpack.json` in a repository can change the provider, toolchain, package manager, base image, start command and build commands [M: F9] | The plan is repository-controlled; the API validates structure and pins images, and does **not** claim to sanitise commands |
| The CI corpus re-derives the plan key union on every Railpack bump | Keeps I18 honest |

## 9. Adversarial acceptance matrix

| ID | Attack | Expected result | Test |
| --- | --- | --- | --- |
| AT-1 | A repository containing a malicious payload is imported and built | The control plane never parses it; the build runs only on the worker; the target receives an image | E2E, FT-1 |
| AT-2 | A build step prints a runtime env secret of its own service | The value never appears in worker events, API logs, `deployment_logs`, artifact metadata or diagnostics | FT-9 |
| AT-3 | The worker presents a stolen/other worker token, or a session cookie, to the gateway | Rejected; scope mismatch rejected; a session cookie is never accepted on the gateway | AT-3 test |
| AT-4 | A build step tries to reach the control plane, the database or another tenant | Every probe refused; no DNS answer for the control-plane name | FT-4, FT-5 |
| AT-5 | A build step tries the cloud metadata service | Refused by IP and by name (NXDOMAIN) | FT-5 |
| AT-6 | A service has a build-time secret configured | The build is refused with `build_secret_required`; the secret never enters the build | AT-6 test |
| AT-7 | A repository's `railpack.json` tries to inject a build command, a start command, a base image or a registry | Commands are accepted (the plan is repository-controlled by design) but images are digest-pinned by the API; the injected command executes only inside the sandbox | AT-7 test |
| AT-8 | A build tries to poison the BuildKit cache for a later job | No cache survives a job (per-job builder); the later job is unaffected | FT-3 |
| AT-9 | A build tries to read the worker's filesystem outside the job tmpfs | Refused; the worker's filesystem is read-only except the job tmpfs | FT-3 |
| AT-10 | A paused worker wakes up after its lease expired and tries to write | Rejected with `stale_fence`; a stale artifact upload is deleted | FT-6 |
| AT-11 | The network between the worker and the gateway is cut mid-build | The build stops within the lease window; the job is re-queued; exactly one completion | FT-6 |
| AT-12 | A build prints ANSI escapes, control characters or a forged log line | Stripped and stored as data; no log-forging of the API's own output | AT-12 test |
| AT-13 | The artifact is corrupted, truncated or replayed in transit | Detected; the job fails with a distinct code; no partial artifact remains | FT-8 |
| AT-14 | An application container tries to reach the monitoring agent | Dropped by the network separation | F5 + E2E |
| AT-15 | The worker is unavailable when a deployment is requested | The deployment fails closed with `build_worker_unavailable`; it is **not** redirected to a target-side build | E2E |
| AT-16 | Railpack is upgraded and the plan schema changes | CI fails until the allowlist is updated | CI gate |
| AT-17 | A rogue operator adds the worker to the customer host instead of a dedicated worker | Refused by the topology requirement: the worker must have no route to the control plane except the gateway; the API's gateway endpoint is not reachable from the customer target | Operator runbook |
| AT-18 | The worker's token is revoked while a build is running | The lease is voided; the worker's next heartbeat fails; the build is terminated; the job is re-queued | FT-6 + revocation test |

## 10. Feasibility test plan

The full plan with pass criteria is in the feasibility file (FT-1 … FT-10).
Three additions follow from this revision:

| ID | Test | Pass criterion |
| --- | --- | --- |
| FT-11 | Measure a successful build end to end on a worker VM: BuildKit state peak, artifact size, wall time, memory, CPU | Peak BuildKit state ≤ 8 GiB, artifact ≤ 2 GiB, build ≤ 30 min for the 9-provider corpus, RSS ≤ 8 GiB; the numbers are then written back into this RFC's resource table |
| FT-12 | Verify the recommended W1 configuration (native rootless `buildkitd`, no Docker daemon on the host): a `RUN` step executes, `docker` is not installed, no host socket exists, and the build steps cannot see the host filesystem | All four hold; otherwise fall back to W2 and record the accepted risk |
| FT-13 | Verify the recommended "API relays the tarball" acquisition mode: the worker's egress set is reduced to the gateway only, and the tarball digest mismatch is detected | The worker's firewall admits exactly one destination; a corrupted relayed tarball is rejected before extraction |

**Release blockers for Phase 1:** FT-2, FT-3, FT-4 (and FT-12 as the
configuration gate).

## 11. Phased implementation plan

| Phase | Contents | Entry criteria | Exit criteria |
| --- | --- | --- | --- |
| **Phase 0 — remove the unsafe paths without breaking production** | (a) Delete the target-side source build path [C: `api/jobs/deploy.go:415-454`]; (b) add `build_jobs` and `worker_tokens`; (c) add the plan policy + the 9-provider CI corpus; (d) move runtime env values off the target's argv; (e) move the monitoring agent to its own Docker network; (f) add the `SOURCE_BUILD_MODE` switch with modes `legacy` \| `worker` \| `disabled` | RFC approved | The target-side build path is gone; the corpus CI gate is green; the switch defaults to `legacy` (no behaviour change for existing installations) |
| **Phase 1 — the worker** | Worker agent, per-job rootless `buildkitd`, the gateway service, the six-endpoint protocol, lease/fence/heartbeat, the artifact protocol, deny-by-default egress, the relay-based acquisition, build-time secrets off, `analyze`/`from-source` served by `prepare` jobs | Phase 0 done; FT-2, FT-3, FT-4, FT-12, FT-13 passed on a real worker VM | The 9-provider corpus builds through the worker (FT-10); AT-1…AT-18 pass; a canary deployment to a disposable target succeeds |
| **Phase 2 — assurance** | Signed provenance (in-toto/SLSA-shaped) with a documented trust root; per-workspace cache (with cache-key isolation); the authorised build-secret mechanism; an optional trusted tier for first-party repositories | Phase 1 in production | The provenance record is verifiable by an external verifier |
| **Phase 3 — scale and maturity** | Multi-arch workers; per-tenant workers; autoscaling; queue fairness | Phase 2 | An arm64 customer can deploy from source |

## 12. Migration, activation, rollback and failure strategy

### 12.1 Activation (the rollout that does not break production)

The tension: removing the target-side build path (Phase 0a) breaks source
deployments, but the worker does not exist until Phase 1. Resolution: the
switch, plus a **capacity precondition**.

1. Phase 0 ships with `SOURCE_BUILD_MODE=legacy` — behaviour is unchanged for
   every existing installation.
2. Phase 1 ships the worker in **shadow** mode: the worker performs acquisition
   and preparation and returns a candidate plan; the API compares it with the
   plan the control plane produces and records the differences. Shadow mode
   produces no artifacts and changes no behaviour; it exists to measure
   plan-equality, duration and resource consumption. **Shadow mode still runs
   `railpack prepare` on the control plane**, so it does not reduce risk — it
   is a rehearsal, and it must not be described as a mitigation.
3. **Activation precondition** (checked by the API, not by the operator's
   hope): at least one worker is `ready` and has completed a successful
   9-provider-corpus build within the last 24 h; the reference fixture builds
   end to end; the artifact path is verified. If the precondition fails, the
   activation is refused with a clear message.
4. Activation flips `SOURCE_BUILD_MODE` to `worker` for one installation at a
   time. There is **no automatic fallback** to `legacy` (G6): if the worker
   becomes unavailable, deployments fail closed with `build_worker_unavailable`
   (AT-15).
5. `disabled` is available as an operator-selected, fail-closed mode: source
   deployments are refused; **already-running services keep serving** (no
   outage), and no build of any kind occurs.

### 12.2 Rollback

| Direction | Mechanism | Re-introduces the vulnerability? |
| --- | --- | --- |
| Phase 1 → Phase 0 (worker path broken) | `SOURCE_BUILD_MODE=disabled` (fail closed) or `legacy` | `disabled`: **no** — builds stop, running services are unaffected. `legacy`: **yes** — this re-enables the target-side build and the control-plane preparation; it requires an explicit, recorded, time-boxed risk acceptance by the operator. |
| Code rollback past Phase 0 | Deploy the previous release | **Yes** — the previous release contains the target-side build path. Documented as a risk-accepting action, not a routine rollback. |
| Code rollback *within* Phase 1 | Deploy the previous Phase-1 build | No — the Phase-0 deletions are already in the tree, so the target-side build path stays deleted. |
| Worker host lost | Replace the worker host; revoke the old token; re-queue its jobs | No |

**Preferred rollback, stated plainly:** `SOURCE_BUILD_MODE=disabled` plus a
replacement worker. The fallback to `legacy` exists only so that an operator
can make a deliberate, documented risk decision; it is never automatic, never
the default, and never triggered by a build failure.

### 12.3 Failure modes and the behaviour for each

| Failure | Behaviour | Data impact |
| --- | --- | --- |
| Worker host dies mid-build | Lease expires; the API re-queues (≤ 3 attempts) and then fails the deployment with `worker_lost` | None; no artifact was stored |
| Worker unreachable (network) | Same as above; the API's health check marks the worker down | None |
| Build fails (compile error, missing lockfile) | The job fails with the build's exit status; the output is redacted and stored; the deployment ends `failed` | None |
| Build exceeds the deadline | The API marks the job failed and increments the fence; the worker's next heartbeat stops the build | None |
| Artifact transfer fails | The job fails; the partial file is deleted | None |
| Target-side `docker load` fails | The deployment fails; the target keeps the previous image and container | The previous service keeps running |
| Target-side image ID mismatch | The deployment fails with `target_image_mismatch`; the image is not started | The previous service keeps running |
| Plan rejected by the API | The job fails with `plan_rejected` and the reason; no build runs | None |
| API restarts mid-deployment | The deployment reconciler [C: `api/jobs/deployment_reconciler.go`] re-evaluates; the job is re-queued or failed; no duplicate deployment | None |
| Gateway compromised | Revoke the worker token; the API treats the gateway's assertions as worker-level; re-queue onto a fresh worker | Images from the compromised gateway are suspect (§4.9) |

## 13. Operational and resource requirements

### 13.1 Worker host

| Resource | Value | Basis |
| --- | --- | --- |
| CPU | 4 vCPU | The hosted SZ-09 Lite build of the reference fixture took 51 s on 4 vCPU |
| RAM | 8 GiB | BuildKit plus a Bun/Go/Rust toolchain build; the API's own limit is unrelated |
| Disk | 60 GiB (40 GiB usable) | Per-job BuildKit state ≤ 8 GiB [M: F8 measured 1.7 GiB for a partial tiny build], artifacts streamed and not stored on the worker, plus headroom for the toolchain images (239 MiB compressed for `railpack-builder` [M: F8]) |
| Job tmpfs | 2 GiB | The existing extracted-source cap is 512 MiB [C: `api/source/source.go:525-527]; the remainder is headroom for toolchain staging |
| BuildKit state per job | 8 GiB hard cap | To be confirmed by FT-11 |
| Artifact | ≤ 2 GiB, streamed | To be confirmed by FT-11 |
| Concurrency | 2 jobs per worker | Matches the current analysis concurrency default [C: `api/source/limits.go:64-69] |
| Network | 1 inbound (gateway), outbound deny-by-default | §4.4 |
| OS | A mainstream Linux with working user namespaces | FT-2 / FT-12 |

### 13.2 Control plane changes

| Change | Cost |
| --- | --- |
| `build_jobs` + `worker_tokens` tables | Two new tables; the migration-integrity manifest must be updated (§14, B5) |
| The gateway as a separate process | One more binary, one more unit, one more configuration surface |
| A reaper goroutine (30 s) | Negligible |
| The artifact volume | 2 GiB per job, 24 h TTL, per-workspace quota; encrypted |
| The `SOURCE_BUILD_MODE` switch | One environment variable with three values |

## 14. Operator decisions and remaining blockers

### 14.1 Decisions that need an operator

| ID | Decision | Recommendation | Conflict with current requirements? |
| --- | --- | --- | --- |
| Q1 | Trusted tier for first-party repositories (bypass the worker)? | **No tier in v1.** Every source build goes through the worker | Fewer options for the operator; no product conflict |
| Q2 | Who enforces the worker egress policy? | **The operator's firewall is authoritative**; the API's per-provider list only narrows it | The API cannot verify the firewall ⇒ a mismatch fails the job closed |
| Q3 | Worker identity: mTLS + token, or token only? | **mTLS + token**; token only is the minimum | Requires the operator to issue client certificates |
| Q4 | Does `POST /api/source/analyze` stay synchronous? | **Yes**, via a bounded wait on a `prepare` job (the current UX is synchronous, and the analysis timeout is 90 s) | A slow worker now shows as an analysis timeout instead of a CPU-bound wait |
| Q5 | Where do artifacts live? | **An encrypted volume on the API host**, 24 h TTL, per-workspace quota | A control-plane disk cost; an alternative (object storage) adds a credential to the control plane |
| Q6 | Are build-time secrets needed? | **Off by default.** If a service needs one, the build is refused with a clear error | **Yes — product-visible.** Repositories needing `NPM_TOKEN`/private indexes stop building. Must be communicated |
| Q7 | Private registries? | **Deferred** until the authorised build-secret mechanism exists | Customers using private registries cannot use source builds |
| Q8 | Worker architecture | **amd64 only in Phase 1**, with an explicit `unsupported_platform` error | **Yes — product-visible.** StageZero's own image ships arm64 [C: `api/Dockerfile`], so arm64 customers lose source builds until Phase 3 |
| Q9 | Should the API constrain the toolchain version? | **Yes — minimally.** The API may require the plan's resolved toolchain versions to satisfy the same 14-day minimum age Railpack already applies | Constrains what can be built; prevents a repository from pinning a brand-new toolchain release |
| Q10 | Queue depth and deadlines | prepare 90 s, build 30 min, queue depth 20, worker concurrency 2 | Matches the current limits [C: `api/source/limits.go`, `api/jobs/deploy.go:36-37] |
| Q11 | Is the API's `git ls-remote` SHA resolution kept? | **Yes** — it is the control plane's independent source check and costs one read-only call | None |
| Q12 | Who may activate `SOURCE_BUILD_MODE=worker`? | **An operator with the installation's admin role**, with an audit record | None |

### 14.2 Blockers before Phase 1 can start

| ID | Blocker | Owner | Why it blocks |
| --- | --- | --- | --- |
| B1 | FT-2: rootless BuildKit runs real Railpack build steps on a real VM | Platform | The recommended worker configuration is unproven; the fallback (W2) accepts a privileged builder |
| B2 | FT-3/FT-4: a hostile build step cannot reach the host or the control plane, and egress is deny-by-default | Platform | The core isolation claim of the RFC |
| B3 | FT-11: artifact and BuildKit-state sizes on a real worker | Platform | The resource and limit numbers in §13 are estimates |
| B4 | SSH host-key verification is still `InsecureIgnoreHostKey()` [C: `api/ssh/client.go:94`] | Security | A P0 in its own right; SZ-12 does not fix it, and the target-side deployment path it protects remains MITM-able |
| B5 | The migration-integrity manifest [C: `api/db/migrations/manifest-0031.json`] must be extended when `build_jobs` and `worker_tokens` are added | Backend | SZ-11B's fail-closed gate will otherwise reject the new migrations |
| B6 | Product decision on build-time secrets and on arm64 targets (Q6, Q8) | Product | Both are user-visible regressions if the recommendation is accepted as-is |
| B7 | The gateway's deployment model (same host as the API, or a separate host) | Operator | Determines the network policy and the credential scope in detail |

## 15. Product conflicts flagged

| Conflict | Consequence of the recommended default | Mitigation |
| --- | --- | --- |
| Build-time secrets off (Q6) | Repositories needing a build token stop building | Explicit `build_secret_required` error; Phase 2 mechanism; documented migration note |
| amd64-only workers (Q8) | arm64 customers lose source deployments | Explicit `unsupported_platform` error; Phase 3 multi-arch workers; a per-architecture worker pool |
| No target-side fallback (G6) | An installation without a ready worker cannot deploy from source | Fail closed with a clear error; a single-node worker appliance; shadow mode to rehearse before activation |
| Derived egress allow-list (Q2) | Corporate proxies and private registries break | Private registries deferred; the operator may widen the firewall, and the API's list only narrows |
| Source deploys are now asynchronous under the hood | The `analyze` endpoint's latency now includes a worker round trip | Bounded wait, the same 90 s budget; the operator's worker is on the same LAN or reachable |
| The `railpack.json` finding (F9) | A repository can choose its own build command and start command | Accepted: the runtime start command was always repository-defined; the *build* now happens on a disposable worker instead of the customer's host |

## Appendix A — glossary

| Term | Meaning |
| --- | --- |
| Control plane | The API host: master key, database, tenant data, plan validation |
| Worker | The build worker host: acquires the source, prepares it, builds the image; holds no credentials |
| Gateway | The separate process that authenticates workers and forwards six message types |
| Fence | The monotonically increasing token that invalidates a superseded worker's writes |
| Candidate plan | The plan a worker produced from repository content — untrusted |
| Approved plan | The plan the API validated, canonicalised and digest-pinned — the only plan that may be built |
| Job | One unit of work: `prepare` or `build` |
| W1 / W2 / W3 | The three worker/BuildKit configurations (§4.2) |
| P-A…P-D | The four network phases (§4.4) |

## Appendix B — change log

| Revision | Change |
| --- | --- |
| Rev 1 | Initial architecture: dedicated worker, deny-by-default egress, lease/fence protocol, artifact transfer, build-time secrets off, plan policy, phased plan |
| Rev 2 (this document) | Ten inconsistencies resolved: inverted preparation direction (§4.1); the stock driver's privileged builder withdrawn in favour of native rootless BuildKit (§4.2); the gateway isolated as a separate process with no tenant credentials (§4.3); four separate network policies (§4.4); the full lease/fence/cancellation/retry/idempotency specification (§4.5); build-time secrets removed rather than flagged (§4.6); AT-2 rewritten around the API-side redaction set (§4.7); the API↔worker boundary stated as a capability, resolving the token/credential contradiction (§4.8); the consequences of a compromised worker stated without claiming checksums prove honesty (§4.9); the runtime-container risk bounded to one surgical change, with the rest deferred to SZ-13 (§4.10) |
