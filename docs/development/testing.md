# Testing and verification

Run from the repository root unless a different directory is shown. `main` CI is `.github/workflows/ci.yml`; these commands do not substitute for a database-backed upgrade rehearsal or a live remote-host test.

| Check | Command | Scope |
| --- | --- | --- |
| Go tests | `cd api && go test ./...` | Unit and existing handler/package tests. |
| Go vet | `cd api && go vet ./...` | Static checks. |
| Go race tests | `cd api && go test -race ./...` | CI runs this; useful for in-process jobs. |
| Frontend types | `bun run --cwd frontend check` | Svelte/TypeScript. |
| Frontend lint | `bun run lint` | Prettier and ESLint. |
| Frontend build | `bun run build:web` | Compilation, **not** proof of a production serving adapter. |
| Local setup tests | `bun test scripts/setup.test.ts` | Setup guard and fixture tests. |
| Contract drift | `bun run generate && git diff --exit-code -- api/gen/api.gen.go api/db/sqlcgen frontend/src/lib/api/v1.d.ts` | Requires pinned `sqlc` and `oapi-codegen`; do not run with uncommitted generated-file work you want to preserve without checking the diff first. |

`bun run check` combines Go vet and frontend check; `bun run test` combines Go tests and setup tests. `bun run build` combines Go and frontend builds. CI installs Bun 1.4.3, Node 22, Go from `api/go.mod`, pinned generators, and checks all of the above including race tests and codegen drift. It does **not** start PostgreSQL, create a remote Docker server, test migrations on existing data, drive a browser, verify OAuth callbacks, or verify alert delivery.

For schema changes, use an isolated, backed-up DB: apply all migrations to a clean DB **and** a snapshot with preexisting data, verify constraints and reads, and record rollback/data-loss expectations. In particular, `0018` adds non-null project/environment references without a data backfill. For deployment changes, use a disposable remote host and real DNS/HTTPS to test both no-domain replacement and domain-based candidate/cutover/rollback; confirm DB status, remote containers, logs and SSE reconnect. Do not run these checks against a production server or reset its DB as a shortcut.

For feature branches, add end-to-end gates before merge: [GitHub import](../features/github-import.md#verification-gates) needs a real repository, Railpack and remote Docker; [metrics](../features/metrics.md#verification-gates) needs the agent, history, alert dispatch and failure/recovery cases. [Branch audit](../audits/branch-audit.md) distinguishes checks actually executed from proposed tests.
