# SZ-11B Migration Integrity Implementation Plan

> **For agentic workers:** Implement inline in this session. Keep every edit and disposable database in the isolated combined candidate; no delegation or production access.

**Goal:** Refuse migration runs with an incomplete or altered combined 0001–0031 bundle, and detect incomplete effective history or broken critical schema after Goose runs.

**Architecture:** Merge the two feature tips into an integration base without changing either feature branch. In a focused child branch, pin the exact embedded SQL bundle before Goose and verify effective PostgreSQL state after Goose, including out-of-order history. Keep normal Down behavior unchanged but explicitly prohibit it as a release rollback.

**Tech stack:** Go, embedded FS, SHA-256, Goose v3.27.0, PostgreSQL 18, sqlc v1.30.0, oapi-codegen v2.6.0, Bun 1.4.3.

## Global constraints

- Base: `origin/feat/github-import` `64758d22ffe2451304973e20a8064ed54ac3a59f` and `origin/feat/metrics-mvp` `65e0873fa0868123e1c18add52a242ebaef01467`; do not push to either branch or `main`.
- Bundle: exactly SQL versions 0001–0031; source owns 0026 and 0031, Metrics owns 0027–0030. Do not rewrite existing SQL migration bytes or applied history.
- Gate belongs only in combined candidate. Preserve source security and monitoring behavior, no new runtime dependencies, no live-host or production operations.
- Disposable PostgreSQL URL is opt-in (`STAGEZERO_TEST_DATABASE_URL`). Use `TMPDIR=GOTMPDIR=/home/radityra/.cache/opencode-go-tmp` for Go tests.

## File map

- `api/db/migrate.go`: wire preflight and postflight around the existing Goose Up; leave Down untouched.
- `api/db/migration_integrity.go`: validate embedded manifest, effective history and catalog structure; use the same embedded FS as Goose.
- `api/db/migrations/manifest-0031.json`: deterministic ordered list of 31 filename/digest pairs. `api/db/migration_integrity.go` pins its own digest.
- `api/db/migration_integrity_test.go`: mutation-driven preflight tests and opt-in PostgreSQL postflight/upgrade tests.
- `docs/superpowers/specs/2026-10-21-sz11b-migration-integrity-design.md`: approved contract, already committed.
- `docs/superpowers/specs/2026-10-17-sz09-import-railpack-verification.md`: no changes; SZ-09 stays a separate, authorized smoke gate.
- `docs/superpowers/specs/2026-10-21-sz11b-migration-integrity-design.md` or a short adjacent release note: explain unsafe Down, backup/restore and future numbering.
- Six conflict files: `api/config/config.go`, `api/gen/api.gen.go`, `api/jobs/deploy.go`, `api/main.go`, `api/ssh/shell.go`, `frontend/src/routes/(app)/(builder)/+layout.svelte`; also inspect every semantic auto-merge and regenerate API clients.

### Task 1: Build combined candidate

**Interfaces:** Consumes the two remote feature tips; produces a merge commit on `integration/sz11b-candidate`, never on either source branch.

- [ ] Fetch both tips; compare against pinned SHAs. Merge `origin/feat/metrics-mvp` without committing until all conflicts are inspected. If either tip moved, inspect new commits before continuing.
- [ ] Resolve each of the six conflicts by retaining both sides' behavior: source limits/ref/SHA deploy and monitoring state/alerts/reconciliation, SSH hardening plus agent shell, both config and UI bindings. Record the choice and evidence for every conflict in the merge commit/PR description.
- [ ] Inspect auto-merged `api/openapi.yaml`, `api/db/service_config.go`, handler changes, `api/jobs/deploy_test.go`, and generated Go/TS output. Regenerate using `bun run generate` with pinned tools rather than hand-editing generated code.
- [ ] Run `go test ./...` and `go vet ./...` in `api`, `bun run --cwd frontend check`/`lint`/`build`, `git diff --check`; commit the resolved merge; push only `integration/sz11b-candidate`. Create `fix/sz11b-migration-integrity` from that tip.

### Task 2: Bundle preflight (red then green)

**Files:** Create `api/db/migration_integrity.go`, `api/db/migration_integrity_test.go`, `api/db/migrations/manifest-0031.json`; modify `api/db/migrate.go`.

**Interfaces:** `checkMigrationBundle(fs.FS) error` takes the exact filesystem passed to Goose. `RunMigrations(databaseURL string) error` calls it before `sql.Open`/Goose. A valid manifest has ordered objects with `file` and lowercase 64-hex-character `sha256`, one for each version 1..31.

- [ ] Write table tests using `fstest.MapFS` copied from the embedded files: valid bundle; deleted `0028_alerts.sql`; changed SQL byte; changed manifest byte; unexpected SQL; duplicate version/above 31. Assert descriptive errors and no DB work on preflight failure (call `RunMigrations` with a deliberately invalid URL after swapping a test seam only if that seam actually uses the bundle; otherwise test the exact helper called before `sql.Open`). Run `go test ./db -run TestMigrationBundle -count=1` and see failures.
- [ ] Generate the manifest from `sha256sum api/db/migrations/*.sql` sorted by filename, then calculate SHA-256 of the exact JSON file and pin that constant in Go; reject unknown manifest fields, duplicate entries, missing/extra files, bad version ordering, bad digest and any unexpected SQL in the embedded FS. Embed both `migrations/*.sql` and `migrations/manifest-0031.json` in `api/db/migrate.go`; run the bundle check at the start of RunMigrations. Repeat the focused test; expect green.
- [ ] Commit this independently testable gate after `git diff --check`.

### Task 3: Effective history and schema postflight (red then green)

**Files:** Modify `api/db/migration_integrity.go`, `api/db/migrate.go`; extend `api/db/migration_integrity_test.go`.

**Interfaces:** `checkMigrationState(ctx context.Context, db *sql.DB) error` queries the latest row per version by highest `id` from `goose_db_version`, ignores bootstrap version 0, requires each 1..31 effective `is_applied=true` and no other nonzero versions. PostgreSQL catalog checks use `pg_catalog` to verify named columns/tables, FK/PK/CHECK/unique indexes, and absence of `servers.monitoring_private_address`.

- [ ] Add opt-in disposable-PostgreSQL tests for a complete DB, a latest-row `is_applied=false` for 0028, a missing 0028 row, an extra 0032 row, and a dropped active-alert unique index or monitoring check. Assert actionable failures. Run the focused tests against `STAGEZERO_TEST_DATABASE_URL` and observe red.
- [ ] Implement the latest-row query and exact set comparison; query catalogs for `service_sources`, `source_project_requests`, `notification_channels`, `alert_rules`, `alert_rule_channels`, `alert_events`, `servers` monitoring columns, FK/PK/CHECK constraints, and unique monitoring/active-alert indexes. Require absence of the dropped column. Call postflight immediately after successful `goose.Up`. Do not mask Goose failure or auto-rollback; run the focused tests to green.
- [ ] Commit after local tests and `git diff --check`.

### Task 4: Four upgrade paths and negative integrity checks

**Files:** Extend `api/db/migration_integrity_test.go`; add a short rollback and version-policy section to the approved SZ-11B spec or adjacent release note.

- [ ] In disposable PostgreSQL 18, run fresh combined, populated main v25→combined, populated Metrics-first v30→combined, populated source-first v31→combined. Seed two workspaces with users/servers/services; preserve source/idempotency rows and Metrics alert/monitoring rows where those schemas exist. Verify effective 1..31 set, data counts, FK/unique/CHECK rejection and a second no-op RunMigrations. Negative case: omit 0028 from the same test FS and assert no Goose changes; mutate one SQL byte/manifest byte; alter latest applied state; drop an index/constraint and assert failure.
- [ ] Document: `goose.WithAllowMissing()` permits out-of-order upgrades and missing versions, and `goose.Down` rolls back the last applied migration, not necessarily the highest number. Down 0031 deletes durable replay responses. Release rollback means verified backup restore into an isolated DB, compare data/history, then plan forward repair; never ordinary Down on populated release data.
- [ ] Re-run full API test/race/vet/build, frontend check/lint/build and setup tests, pinned generation/drift checks, `git diff --check`. Restore/stop only owned disposable databases/containers and remove temporary dependency symlinks. Commit docs and tests.

### Task 5: Review and publish

- [ ] Compare the combined candidate against both feature tips and record every conflict resolution plus semantic auto-merge tests. Confirm `main`, source and Metrics tips unchanged.
- [ ] Push only the child fix branch. Search the repository for a PR template, open one focused PR targeting `integration/sz11b-candidate`, watch backend/frontend/OpenAPI checks and report their exact outcomes, remaining risks, and the separate SZ-09 authorization gate. Do not merge this PR or the candidate into `main`.

## Self-review

- Coverage: integration, preflight, postflight, four upgrade orders, negative tests, rollback guidance, CI and PR all have explicit tasks.
- Scope: no SQL rewrite, production access, automatic recovery, UI redesign or unrelated dependency.
- Check signatures before implementation: preflight operates on `fs.FS`; postflight operates on `*sql.DB`; RunMigrations remains the single caller boundary.
