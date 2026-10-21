# SZ-11B: migration rollback and integrity checks

This procedure applies only to the combined candidate containing SQL migrations 0001–0031. The combined runner checks the embedded, SHA-256-pinned inventory **before** Goose runs and checks the effective migration history and critical PostgreSQL schema **after** Goose runs. A postflight error does not undo already committed migrations. Stop startup and investigate it; do not edit `goose_db_version` to silence the error.

## Release checks

1. Take and verify a database backup before upgrading. Keep the backup, the application revision, and the migration manifest together. Confirm that the backup restores into a separate, disposable database.
2. Rehearse the exact starting history in an isolated PostgreSQL 18 database: fresh, populated main (through 0025), Metrics-first (through 0030 without 0026), or source-first (0026 and 0031 without 0027–0030). Run the combined candidate's `DATABASE_URL=<disposable database URL> go run ./cmd/migrate up` from `api/`. Never use a production URL for rehearsal.
3. Inspect the command's exit status. Success means the bundle matched the pinned manifest, Goose completed, and the **latest** `goose_db_version` row for each version 1–31 is applied. It also means the checked source and monitoring tables, constraints, columns and indexes exist. Goose's displayed current version alone is insufficient: an out-of-order upgrade can report 0030 while 0031 remains applied.
4. Compare seeded or restored users, workspaces, services, source idempotency responses, monitoring settings and alert records before and after. Run `up` again and verify it makes no changes. Do not infer data preservation solely from a successful schema check.

## Failure and rollback

- A missing or changed SQL file, changed manifest, or unexpected SQL file fails **before** Goose touches the database. Restore the reviewed application artifact; do not regenerate the manifest against an unreviewed bundle.
- If Goose or postflight fails after SQL ran, preserve logs and a fresh database backup. Compare the effective version history (`SELECT version_id, is_applied, id FROM goose_db_version ORDER BY id`) and catalog objects against the manifest and a disposable reference database. Decide on a reviewed forward repair only after finding the cause. Do not mark missing versions applied by hand.
- **Do not use `go run ./cmd/migrate down` as a release rollback.** Goose can remove the last-applied *lower* version after an out-of-order upgrade while a higher version remains applied. Rolling back 0031 also drops `source_project_requests`, destroying saved idempotency responses. `RollbackMigration` remains available for isolated development only; it is not a safe populated-database recovery tool.
- To recover a release, stop writes, restore the verified pre-upgrade backup to a **separate disposable database**, validate application data and effective migration history there, and plan a controlled backup restore or forward repair with explicit operator approval. Do not restore over a live database or run production operations as part of this checklist.

The manifest is an inventory anchored in this application revision, not a signature against a code author who intentionally changes both its contents and pinned digest. The catalog assertions cover selected release-critical objects, not every database object or historical value. A future migration requires a reviewed manifest/version update and new rehearsal.
