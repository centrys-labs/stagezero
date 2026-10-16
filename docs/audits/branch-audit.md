# Branch audit against the default branch

**Snapshot:** 2026-10-16. Configured remote `https://github.com/centrys-labs/stagezero.git`; `origin/HEAD` resolves to `origin/main` at `0e3c0ab8942717cafa3259636e086c90ed89e1fc`. The request also named `radityama/stagezero`; that repository was not the configured remote and could not be inspected with the available GitHub search access. This audit compares the **local branches** to the fetched configured remote's default branch, not to an assumed upstream. Re-fetch/review SHAs before using the conclusions. No feature branch was merged, rebased or pushed during this audit.

| Ref | Audited tip | Relative to `origin/main` | Relationship / state |
| --- | --- | --- | --- |
| `origin/main` | `0e3c0ab8942717cafa3259636e086c90ed89e1fc` | Baseline | PostgreSQL-backed image deploy, no repo build or metrics agent. |
| `feat/github-import` | `1dad5775662eacb041c8a5c0cf5c7055ade4daa0` | 0 behind, 35 ahead | Merge-base is audited main; local tip matches `origin/feat/github-import`. |
| `feat/metrics-mvp` | `7e511e759e4373a0ce3032e36248fc65260cfa4b` | 0 behind, 41 ahead | Merge-base is audited main; `origin/feat/metrics-mvp` is still at `d037dcc` (local merge not pushed). |

No originating feature issue/PR specification was found in the accessible `centrys-labs/stagezero` searches. **Spec fidelity is therefore unverified**; readiness below means code-grounded safety/completeness, not acceptance against an unpublished brief. The local source and monitoring branches were reviewed independently against `main`. A dry-run `git merge-tree --write-tree feat/github-import feat/metrics-mvp` reported conflicts in `api/gen/api.gen.go`, `api/jobs/deploy.go`, `api/ssh/shell.go` (add/add) and `frontend/src/routes/(app)/(builder)/+layout.svelte`. It did not modify a worktree. The migration numbers do not collide (`0026` vs `0027`–`0029`), but schema/code generation and deployment labels need integration tests if both are combined.

## Readiness matrix

| Capability / gate | `main` | `feat/github-import` | `feat/metrics-mvp` |
| --- | --- | --- | --- |
| Core image-based deployment | Implemented; remote cutover not smoke-tested here | Retained and extended for source images | Retained and instrumented |
| Repository analysis/build | Not present | Public GitHub URL, SHA resolve, Railpack detect/build, service source schema; security and live-build gates open | Not present |
| Server/container metrics and history | Not present | Not present | Agent + SQLite retention, API + charts; long-lived deployment history incorrect |
| Alert rules/delivery | Not present | Not present | Rule/event/channel/notification path; secret exposure and unknown-state alert defects |
| Compile/unit/static checks | Go, frontend, setup PASS | Go, frontend PASS (see below) | API/agent Go, frontend PASS (see below) |
| DB migration on populated database | Not run; `0018` upgrade risk | Not run for `0026` | Not run for `0027`–`0029` |
| Production image / remote Docker, browser and notification smoke | Not run | Not run | Not run; no published agent image verified |
| Merge recommendation | Baseline, not a readiness certification | **Hold** for P0/P1 blockers and live journey | **Hold** for P0/P1 blockers and live journey |

## Actionable findings: GitHub import

| ID | Priority | Evidence and user impact | Acceptance check |
| --- | --- | --- | --- |
| G-1 | **P0** | `api/openapi.yaml` leaves `POST /api/source/analyze` unauthenticated; `api/handlers/source.go::AnalyzeSource` permits 90s work, `api/source/source.go` accepts a 500 MiB compressed archive with no global work limit or cumulative expanded-size limit. Anonymous callers can consume API CPU, bandwidth and disk; archive expansion can exhaust storage. | Anonymous call denied (or an explicitly justified protected policy); bounded concurrency, bytes and timeout verified by concurrent/high-expansion test without residual temp files. |
| G-2 | **P0** | `api/source/source.go::Prepare` passes decrypted service env as Railpack `--env KEY=VALUE` process arguments. Other local processes/process-list readers may see secrets. | Build completes while representative secret is absent from argv, logs and error output; document and test build-time secret transport. |
| G-3 | **P1** | `frontend/src/routes/(app)/(builder)/projects/new/repo/+page.svelte` creates project before service. If service creation fails, retries can leave empty projects; `RepoStarterForm.svelte` may accept `/tree/branch` but `source.ParseRepoURL` drops branch path. | Induced second-request failure leaves a recoverable/reusable project; pasted branch remains selected or prompts for confirmation, never silently builds `main`. |
| G-4 | **P1** | Editable source-service image in `frontend/src/lib/components/app/ServiceWorkspace.svelte`/`api/handlers/services.go` can be saved, while `CreateDeployment` selects a SHA-derived tag instead. | Source-backed image is visibly derived/non-editable or a documented override is honored; test edit -> deploy result. |
| G-5 | **P1** | `api/Dockerfile` includes Railpack v0.37.0; root local setup does not install it. The Dockerfile still uses old `uploy` executable/user naming. | A documented local or containerized real repo -> Railpack -> SSH load -> running container smoke succeeds, with consistent StageZero artifact naming. |

The branch does **not** promise private-repo import, GitHub App install, webhook CI or arbitrary provider support; these are explicitly outside this MVP, not missing implementation.

## Actionable findings: metrics MVP

| ID | Priority | Evidence and user impact | Acceptance check |
| --- | --- | --- | --- |
| M-1 | **P0** | `api/handlers/alerts.go::ListNotificationChannels` calls `notificationChannelResponse` for any workspace session member. Channel config is decrypted and includes webhook destinations/secrets despite manage permissions on writes. | Read API returns redacted configuration for non-managers (and ideally all list clients); tests cover member/manager and secret never appears in response/logs. Review agent tokens passed through remote shell commands in `api/monitoring/agent.go` too. |
| M-2 | **P1** | `api/handlers/metrics.go::GetProjectMetricsHistory` filters deployments by `CreatedAt` before querying agent history. A still-running deployment created before the window vanishes from its current seven-day series; the 100-deployment cap can also exclude relevant history. | An old active deployment with in-window samples is included while markers still reflect actual deployment times; tests use explicit timestamps and >100 deployments where appropriate. |
| M-3 | **P1** | `monitoring/main.go::collector.collect` overwrites latest samples even when Docker sampling errors and `/health` stays green; `api/jobs/alerts.go` treats missing service samples as stopped. Conversely `api/alerts/evaluator.go::violation` says unreachable is not a CPU/memory/disk/service violation and `Tracker.Evaluate` resolves an active alert. | Failed sampling yields an explicit unknown/unreachable status; no false service-down or false recovery; only a fresh healthy observation resolves an incident. Cover Docker error, stale sample and recovery. |
| M-4 | **P1** | `api/handlers/monitoring.go::EnableServerMonitoring` writes tokens before remote provisioning; on error it rolls back with possibly canceled request context and ignores rollback failure. Reader-token rotation updates remote agent before DB persistence with no compensating action. | Inject request cancellation, SSH error and DB write error; stored status/tokens and remote container/route converge or show explicit repairable failure; old reader token is not silently stranded. |
| M-5 | **P2** | `monitoring/main.go::historyBands/queryHistoryBand` and rollup buckets may omit boundary points; frontend project metrics page places samples at equal index spacing but markers by wall time. Agent history errors can be logged yet returned as HTTP 200 with empty points. | At retention/rollup boundaries every sample appears once, visual marker aligns with wall-time sample, and agent failures are represented as error/stale rather than no data. |
| M-6 | **P2** | `api/db/alerts.go` persists rules and channel links in separate operations; failure may leave partially linked rules. Alerts UI can create rules but lacks the API-supported disable/delete lifecycle. No live image, alert dispatch or recovery smoke was observed. | Induced second-write failure leaves no partial rule; UI can manage existing rules with feedback; disposable-agent delivery/restart test recorded. |

## Checks actually executed

| Ref | Results | Limits |
| --- | --- | --- |
| `origin/main` docs worktree | `bun install --frozen-lockfile`, API `go test ./...`, API `go vet ./...`, frontend `bun run check`, `bun run lint`, `bun run build`, `bun test scripts/setup.test.ts`: PASS (3 setup tests). | SvelteKit adapter-auto warns that no supported production environment was detected. No `-race` or codegen drift rerun locally. |
| `feat/github-import` detached worktree | API `go test ./...`, `go vet ./...`; `bun install --frozen-lockfile`; `bun run --cwd frontend check`, `bun run --cwd frontend lint`, `bun run build:web`: PASS. | Parallel lint/build attempt timed out under resource contention; sequential rerun passed. No DB, browser, Railpack binary execution or remote host. |
| `feat/metrics-mvp` original worktree | API and `monitoring/` module `go test ./...`, `go vet ./...`; frontend `bun run --cwd frontend check`, `bun run lint`, `bun run build:web`; setup `bun test scripts/setup.test.ts`: PASS. | Parallel build timed out under resource contention; sequential rerun passed. No DB, browser, monitoring image, remote host, public endpoint or real channel delivery. |

Build/test success is narrower than deployment readiness. `go test -race`, pinned generator drift, populated migration tests and live integration remain to run against each future merge candidate. The two branch audits are read-only; findings are proposals in [roadmap](../roadmap.md), not changes silently made to those branches.
