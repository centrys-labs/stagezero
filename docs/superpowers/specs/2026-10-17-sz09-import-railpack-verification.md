# SZ-09: real-world GitHub import and Railpack verification

## Status and boundary

Execution procedure only; no live-host steps have been run. Obtain separate authorization before provisioning or running it. Use **only public, user-controlled trusted fixture repositories** on a fully disposable, isolated API host and target. `railpack prepare` executes on the API host and is **not** confined to its work directory or restricted to git-only network access. The later isolated build-worker boundary for untrusted repositories is a separate release gate; this smoke test does not prove it. Never run this procedure on the production API host, a shared target, or with production credentials.

## Prerequisites and evidence

- Two disposable isolated hosts (API and SSH target), a disposable PostgreSQL 18 database, a test-only StageZero workspace with an owner/developer session, a dedicated test SSH key and verified target host key. No route to production systems or production secret store. Budget at least 2-4 vCPU, 8 GB RAM, and 40 GB disk for the target's build layers. API host needs git and Railpack v0.37.0; **only the target** needs Docker Engine/buildx and curl/tar for source builds. If PostgreSQL is run in a container, its host also needs a container runtime. Allow only the outbound destinations actually required by the pinned fixture and build: GitHub/git/codeload, Railpack runtime/package downloads, and `ghcr.io` for `ghcr.io/railwayapp/railpack-frontend:0.37.0`. Record every additional egress destination rather than claiming an existing sandbox.
- Pin the API's Railpack binary to v0.37.0 (the API Dockerfile installs `railpack-v0.37.0-<x86_64|arm64>-unknown-linux-musl.tar.gz`); `api/jobs/deploy.go` pins the BuildKit frontend **tag** to 0.37.0, not its digest. Record the binary version/digest, frontend digest if available from build logs/registry, API commit, Docker/buildx versions and target architecture. `railpack --version` must report 0.37.0; do not assume its surrounding output format. A mutable frontend tag without an observed digest is residual supply-chain risk, not proof of immutable pinning.
- Two distinct **public fixture repos** under a throwaway account, owned by the operator: (A) a small buildable Node service serving HTTP on port 3000 with a committed lockfile, a `main` commit, and a different commit on `Feature/X`; (B) a recognizable Node project with `package.json`, lockfile, start script and `"build": "node -e 'process.exit(42)'"`. For both, use `"start": "node index.js"`, a checked-in `index.js` that creates an HTTP server on `process.env.PORT || 3000`, and a matching `package-lock.json` (`npm install --package-lock-only` in a local fixture clone). For A, omit the build script. Commit/push both fixtures from the dedicated account before testing and record `git ls-remote` SHAs. Verify `railpack prepare` succeeds for B but its BuildKit build runs the script and fails. A third analysis-negative fixture can be a public repo with no detectable runtime or start command; it must be rejected **before** creation. Do not use third-party or untrusted repos, and never use an analysis-rejected repo to assert deployment-build behavior.
- Operator access: test GitHub-account write access for fixtures; authorized test workspace owner/developer login cookie in a **local, mode-600 cookie jar**; isolated database read access for assertions (and migration rights for setup); SSH to the disposable target with a verified host key and Docker/buildx privileges. Provision test-only app secrets only if testing secret handling; never put them in the shell history, curl body, or shared logs. Use `jq`, `curl`, `psql`, `git`, `ssh`, `uuidgen` (or `/proc/sys/kernel/random/uuid`) from the operator machine. Preflight target firewall/known-host enrollment without disabling host-key checks (SSH known-host rollout is a separate release gate).

## Set up and record the successful path

Run from a trusted operator shell after separate authorization. Replace values in the first block; keep the evidence directory private. `COOKIE_JAR` is a Netscape-format curl cookie jar obtained by logging into the **disposable** StageZero instance (not a literal Cookie header). `DATABASE_URL` points only to the disposable database. Run the migration command from `api/` in a checkout containing both branches' migrations (0026 source, 0027-0030 metrics, 0031 source idempotency). Do not run it against production.

```bash
set -euo pipefail
export API='https://<isolated-api>' OWNER='<fixture-owner>' REPO='<buildable-repo>'
export SERVER_ID='<registered-disposable-target>' COOKIE_JAR='<local-test-cookie-jar>'
export DATABASE_URL='postgres://<test-user>:<password>@<isolated-db>/<test-db>?sslmode=require'
export TARGET_SSH='<test-user>@<isolated-target>'
EVIDENCE="$(mktemp -d)"; chmod 700 "$EVIDENCE"
KEY="$(uuidgen)"; URL="https://github.com/$OWNER/$REPO/tree/Feature/X"
railpack --version | tee "$EVIDENCE/railpack-version.txt"
git ls-remote "https://github.com/$OWNER/$REPO.git" 'refs/heads/Feature/X' | tee "$EVIDENCE/ref.txt"
SHA="$(cut -f1 "$EVIDENCE/ref.txt")"; test "${#SHA}" -eq 40
DATABASE_URL="$DATABASE_URL" go run ./cmd/migrate up
psql "$DATABASE_URL" -Atc 'SELECT max(version_id) FROM goose_db_version WHERE is_applied' | tee "$EVIDENCE/schema-version.txt"
test "$(cat "$EVIDENCE/schema-version.txt")" = 31
```

Use `-H 'Content-Type: application/json'` for **every** JSON POST. The analysis response includes `sha`; creation returns `project`, `environment`, and `service`, but **not** a deployment SHA. A browser check of the repository starter is optional and does not substitute for API assertions.

```bash
jq -n --arg url "$URL" '{repo_url:$url,branch:"Feature/X",branch_confirmed:true}' > "$EVIDENCE/analyze.json"
curl -sS -b "$COOKIE_JAR" -H 'Content-Type: application/json' -o "$EVIDENCE/analysis.json" -w '%{http_code}\n' \
  -d @"$EVIDENCE/analyze.json" "$API/api/source/analyze" | tee "$EVIDENCE/analysis-status"
test "$(cat "$EVIDENCE/analysis-status")" = 200
jq -e --arg sha "$SHA" '.branch=="Feature/X" and .sha==$sha and (.provider|length>0)' "$EVIDENCE/analysis.json"
jq -n --arg url "$URL" '{repo_url:$url,branch:"Feature/X"}' > "$EVIDENCE/unconfirmed.json"
test "$(curl -sS -b "$COOKIE_JAR" -H 'Content-Type: application/json' -o "$EVIDENCE/unconfirmed-response.json" -w '%{http_code}' -d @"$EVIDENCE/unconfirmed.json" "$API/api/source/analyze")" = 400
jq -n --arg key "$KEY" --arg server "$SERVER_ID" --arg url "$URL" \
  '{idempotency_key:$key,server_id:$server,repo_url:$url,branch:"Feature/X",branch_confirmed:true,name:"sz09-smoke",container_name:"sz09-smoke",container_port:3000,host_port:18181}' > "$EVIDENCE/create.json"
test "$(curl -sS -b "$COOKIE_JAR" -H 'Content-Type: application/json' -o "$EVIDENCE/created.json" -w '%{http_code}' -d @"$EVIDENCE/create.json" "$API/api/projects/from-source")" = 201
PROJECT_ID="$(jq -r '.project.id' "$EVIDENCE/created.json")"; SERVICE_ID="$(jq -r '.service.id' "$EVIDENCE/created.json")"
jq -e --arg project "$PROJECT_ID" '.environment.project_id==$project and .service.project_id==$project and .service.source.branch=="Feature/X"' "$EVIDENCE/created.json"
test "$(curl -sS -b "$COOKIE_JAR" -H 'Content-Type: application/json' -o "$EVIDENCE/replayed.json" -w '%{http_code}' -d @"$EVIDENCE/create.json" "$API/api/projects/from-source")" = 201
cmp "$EVIDENCE/created.json" "$EVIDENCE/replayed.json"
jq '.name="changed"' "$EVIDENCE/create.json" > "$EVIDENCE/changed.json"
test "$(curl -sS -b "$COOKIE_JAR" -H 'Content-Type: application/json' -o "$EVIDENCE/conflict.json" -w '%{http_code}' -d @"$EVIDENCE/changed.json" "$API/api/projects/from-source")" = 409
psql "$DATABASE_URL" -v key="$KEY" -At <<'SQL' | tee "$EVIDENCE/idempotency-count.txt"
SELECT count(*) FROM source_project_requests WHERE idempotency_key=:'key'::uuid;
SQL
test "$(cat "$EVIDENCE/idempotency-count.txt")" = 1
```

The final `psql` check should report `1`, and the workspace project count should increase by exactly one from its pre-run baseline. Capture the API's outbound GitHub/Railpack invocation counts or egress logs while **replaying**; no extra analysis should occur. Byte equality and one DB row alone do not prove no second network call. To simulate a lost response, do **not** assume killing a client before commit succeeded: send one request through a disposable test proxy that discards only its response **after** observing the idempotency row/commit, then resend the exact saved body/key. Expect 201 and identical bytes. If the first request never committed, record that as an inconclusive cancellation test, not a replay pass.

## Build, deploy, failure, and recovery

The deployment handler returns `{"deployment_id":"..."}` with HTTP 200; logs are an SSE stream at `/api/deployments/{id}/logs`, and status is read from `GET /api/services/{serviceID}/deployments`. The API response does **not** expose the image tag. The job stores an encrypted configuration snapshot; inspect the target image tag/container instead of assuming an OCI revision label exists. A source deploy resolves the branch again, so capture `git ls-remote` immediately before each deploy and account for any intervening push.

```bash
jq -n --arg id "$SERVICE_ID" '{service_id:$id}' > "$EVIDENCE/deploy.json"
git ls-remote "https://github.com/$OWNER/$REPO.git" 'refs/heads/Feature/X' | tee "$EVIDENCE/deploy-ref.txt"
DEPLOY_SHA="$(cut -f1 "$EVIDENCE/deploy-ref.txt")"
test "$(curl -sS -b "$COOKIE_JAR" -H 'Content-Type: application/json' -o "$EVIDENCE/deploy-response.json" -w '%{http_code}' -d @"$EVIDENCE/deploy.json" "$API/api/deployments")" = 200
DEPLOY_ID="$(jq -r '.deployment_id' "$EVIDENCE/deploy-response.json")"
curl -sS -N --max-time 2100 -b "$COOKIE_JAR" "$API/api/deployments/$DEPLOY_ID/logs" | tee "$EVIDENCE/deploy.sse"
curl -sS -b "$COOKIE_JAR" "$API/api/services/$SERVICE_ID/deployments" > "$EVIDENCE/deployments.json"
jq -e --arg id "$DEPLOY_ID" 'any(.[]; .id==$id and .status=="success")' "$EVIDENCE/deployments.json"
ssh "$TARGET_SSH" "docker image inspect 'stagezero/$SERVICE_ID:$DEPLOY_SHA' >/dev/null"
ssh "$TARGET_SSH" "docker ps --format '{{.Image}} {{.Names}}'" | tee "$EVIDENCE/target-containers.txt"
```

The log should show `analysing repository...`, `detected ...`, `fetching source...`, `building source image...`, and completion. Confirm the running test container's image equals `stagezero/<serviceID>:<DEPLOY_SHA>` (inspect the exact container, not just an image on disk), and call `curl -fsS http://127.0.0.1:18181/` **on the isolated target**; preflight that port 18181 is free and not publicly exposed. Record the BuildKit frontend image digest from the target's build logs/registry if available, not an assumed label. Push a second commit to `Feature/X` **only after** the first deploy finishes, record the new remote SHA, deploy again, and confirm a new tag/current container. The `main` branch must remain at its different SHA.

Run these as independent fixtures, using a **fresh UUID and unique container name** for every new creation:

1. **Analysis failure:** POST analyze and from-source with the no-runtime fixture; expect 422 and **no project/idempotency row**. A missing branch returns 400; unreachable GitHub returns 502 only if induced safely by test network controls. Do not claim a build happened.
2. **Deployment build failure:** POST analyze for fixture B and require 200, then POST from-source and require 201. POST a deployment; its deliberate Node `build` script exits 42 during target BuildKit execution. Expect an SSE error and `failed` deployment status; confirm no **new** container for B is running, collect target image/build logs, and check `/tmp/stagezero-source-<deploymentID>` was removed. If prepare rejects B, fix the fixture before calling this test passed. No artificial exit-75 claim: observe retryability only if a genuine transient 75 occurs in a separate disposable scenario.
3. **Client cancellation vs job interruption:** use the disposable response-dropping proxy to disconnect the deployment POST client **after the deployment row commits but before its HTTP response reaches the client**; retrieve the ID from the isolated database, then confirm the background job continues (`CreateDeployment` starts it independently of the request context). Disconnect an SSE reader separately; that is not a job-cancellation endpoint. For a genuine timeout, isolate a test-only target network stall or slow **trusted** fixture until the configured **30-minute source-deployment timeout**; expect failure/logs and inspect the target workdir/container. Do not use a huge untrusted repo or assume cleanup succeeded: the deferred workdir removal logs failures. To test process interruption, stop the isolated API process mid-build, restart it, and inspect the interrupted-deployment reconciler result; record if a target BuildKit process/workdir survives. Do not stop a shared service.
4. **Rate/concurrency:** use a fresh test user and repeated authorized requests to `/api/source/analyze` and `/api/projects/from-source` (new keys only) until their shared gate returns 429. Replaying a previously committed key should still return its saved 201 without entering that gate.

## Diagnostics and teardown

Preserve only redacted status bodies, SSE output, API logs, GitHub ref/SHA records, DB counts, target `docker ps` and image inspection, Railpack/BuildKit versions/digests, and workdir checks in the private evidence directory. Never upload cookies, SSH keys, DB URLs, full config snapshots, or secret values to the PR. Record exact timeline and reason for any non-201/200 response. A failed SSH cleanup is a **failed teardown**, not proof of no orphan.

1. Wait for all deployments to reach a terminal status. As a test workspace **owner**, run `curl -sS -b "$COOKIE_JAR" -X DELETE -o "$EVIDENCE/delete-service.json" -w '%{http_code}' "$API/api/services/$SERVICE_ID"` and require 204; repeat with the IDs of other fixture services. The handler removes active containers before deleting the row and refuses deletion while a deployment is in progress. Then run `curl -sS -b "$COOKIE_JAR" -X DELETE -o "$EVIDENCE/delete-project.json" -w '%{http_code}' "$API/api/projects/$PROJECT_ID"` and require 204. Do not delete a project first (its DB cascade does not prove container cleanup). Confirm target test containers and `/tmp/stagezero-source-<deploymentID>` directories are gone with `ssh "$TARGET_SSH" 'docker ps -a; find /tmp -maxdepth 1 -name "stagezero-source-*" -print'`. Explicitly remove only test-tagged images/build cache on the disposable target after saving evidence.
2. Revoke the test session/SSH key, remove the test key from the target's `authorized_keys`, and delete the two/three dedicated fixture repositories. No server/SSH-key deletion API is promised; discard the dedicated workspace/database and hosts rather than altering persistent records.
3. Drop only the disposable database; destroy the isolated API/target hosts, private evidence and dedicated network. Confirm no test credentials, containers, workdirs or images remain. If any check fails, preserve the isolated hosts for investigation rather than exposing or repurposing them.

## SZ-11: migration rehearsal controls

`api/db/migrate.go` uses `goose.Up(..., goose.WithAllowMissing())` globally so a database that reached metrics v30 without source v26 (or source v31 without metrics v27-30) can apply lower-numbered missing migrations. This is **not** an integrity or dependency proof: a missing migration could be accidentally skipped, a reused number could mean different SQL, or an older migration could depend on a later schema. `goose.Down` has no matching allow-missing option in this runner, and rollback may target the most recently applied version rather than the highest number; never assume one `down` undoes v31. New migrations must use unused versions above **both** branch heads; reconcile collisions and inspect `goose_db_version` before rollout. Do not rename or replay an already-applied migration in place.

On a disposable **combined-code checkout** with 0026_service_sources, metrics 0027-0030, and 0031_source_project_idempotency, rehearse (a) clean install, (b) populated main v25 to combined, (c) metrics v30 then combined, (d) source v31 then combined. For each: snapshot/backup the test DB; record `SELECT version_id,is_applied FROM goose_db_version ORDER BY id`, verify all expected applied versions 1-31 appear **exactly once as active** (`count(*) FILTER (WHERE is_applied)`) and schema/row counts for services, source rows, server monitoring, alerts and idempotency; run `go run ./cmd/migrate up` twice and require the second run to be a no-op. Before any rollback rehearsal, identify the actual last applied migration from the version table, test **on a restored copy only**, and compare expected data against a restore rather than running `down` on persistent data. The source-only checkout lacks metrics SQL and the metrics-only checkout lacks 0026/0031, so these paths require staged checkouts with their respective migration sets. Stop on an unexpected missing version, duplicate filename number, migration error, or data loss; do not use production credentials/databases. This plan records residual ordering risk, not approval to merge either feature branch into `main`.
