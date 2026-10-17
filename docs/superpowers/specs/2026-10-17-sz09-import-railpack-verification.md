# SZ-09: real-world GitHub import and Railpack verification

## Status

Execution checklist and plan only. Nothing here has been run yet, and no step in it
has been verified on real infrastructure. It defines what SZ-09 will prove, the
disposable environment it needs, the exact commands, the expected results, and the
teardown. Running it requires separate authorization because it needs a live host,
a live target server, and network egress to GitHub.

## What SZ-09 proves

SZ-08's automated tests use a fake analyzer and a fake target. SZ-09 is the first
check that the whole path works against real GitHub and real Railpack:

1. Analysis returns the real repository identity and resolves the confirmed branch
   to a real commit SHA.
2. Atomic creation writes one project, environment, source service, and source row.
3. A retried request replaying a lost response returns the saved project and runs
   no second analysis.
4. Deployment builds the pinned commit with Railpack v0.37.0 and tags the image with
   that SHA.
5. Build failure, timeout, and cancellation leave no orphaned container and produce
   a clear, actionable log.
6. Teardown leaves no test data, no test credentials, and no running workloads.

## Infrastructure requirements

- **Disposable host**, not the production API host. The API runs `railpack prepare`
  on itself (`api/source/source.go::Prepare`, called from
  `api/jobs/deploy.go::prepareSourcePlan`), so an untrusted build plan is prepared on
  whichever machine runs the API. SZ-09 must not run that on a production host.
  Give the host no production credentials and put it on a throwaway network.
- **Resources:** 2-4 vCPU, 8 GB RAM, 40 GB disk (image layers and two build caches).
- **Docker Engine with the `buildx` plugin** on both the API host and the target
  server. BuildKit must be able to pull the build frontend
  `ghcr.io/railwayapp/railpack-frontend:0.37.0`.
- **Railpack v0.37.0**, pinned, installed from the release tarball the Dockerfile
  uses: `railpack-v0.37.0-<x86_64|arm64>-unknown-linux-musl.tar.gz`, unpacked to
  `/usr/local/bin/railpack`. The API Dockerfile sets `ENV RAILPACK_VERSION=0.37.0`
  and the deploy job hard-codes the same version
  (`api/jobs/deploy.go`, `const railpackVersion = "0.37.0"`). Both must match.
- **git** on the API host. `ResolveSHA` shells out to `git ls-remote`.
- **Disposable PostgreSQL 18** for the API. Migrate to the current head (0031 on the
  combined branch) with `go run ./cmd/migrate up`.
- **Disposable target server** registered in StageZero over SSH, with Docker and
  buildx, its own SSH key, and nothing else running on it.
- **Outbound HTTPS** to `github.com`, `codeload.github.com`, `ghcr.io`, and the
  registry the built image is pushed to or stored in. Nothing else.
- **A test browser session** for the repository starter page, if the UI steps are
  included. The API steps can be driven with `curl`.

## Security controls

- Use a dedicated throwaway GitHub account and a small public test repository. No
  private repositories, no real organization, no personal access tokens.
- Place the API host and target server on an isolated network that cannot reach
  production systems. No production database, no production SSH key, no production
  secret store.
- Inject test application secrets only as the service's environment variables.
  Confirm during the build that `ps` on the target server shows only `sh -s` for the
  build, never the secret values on the argument list (this is what
  `sourceBuildInvocation` exists to guarantee).
- Confirm Railpack analysis on the API host cannot write outside its work directory
  or reach the network beyond the git fetch it performs.
- Bound the test run to the disposable account. At teardown, revoke the SSH key and
  session tokens and delete the test repository.

## Test data

- One repository with a commit that Railpack can build end to end, for example a
  small Node or Go service that serves one HTTP response. Record the exact commit:
  `git rev-parse HEAD` (`<SHA>`).
- A branch whose name contains a slash, for example `Feature/X`, pointing at a
  different commit than `main`, so branch resolution is observable.
- GitHub URLs to exercise: the plain repo URL, a `/tree/Feature/X` URL at that
  branch, and a `/blob/Feature/X/README.md` file URL, plus one `/tree/Feature/X/src`
  URL that is a branch plus a directory and so must still be confirmed as
  `Feature/X`.
- A second repository that Railpack cannot build (no recognizable runtime), to
  exercise the failure path.

## Commands and expected results

Run these in order. `<API>` is the disposable API base URL, `<SERVER_ID>` the
registered target, `<KEY>` a fresh UUID.

1. `railpack --version` on the API host prints `0.37.0`. A different version is a
   stop condition: analysis and build frontend would disagree.
2. `DATABASE_URL=<disposable> go run ./cmd/migrate up` applies through version 0031
   and creates `source_project_requests`. Re-running it applies nothing.
3. `curl -sS -b "$COOKIE" -X POST <API>/api/source/analyze -d
   '{"repo_url":"https://github.com/<owner>/<repo>/tree/Feature/X","branch":"Feature/X","branch_confirmed":true}'`
   returns 200 with the repository identity and a 40-character SHA equal to the
   `Feature/X` head. Without `branch_confirmed`, expect 400 and no analysis.
4. `curl ... /api/projects/from-source` with a fresh `<KEY>`, the same URL, and the
   service fields returns 201 with `project`, `environment`, and `service`, and
   `service.source.branch == "Feature/X"` with the original case. Exactly one project
   row exists for the workspace.
5. Re-POST the identical body with the same `<KEY>`. Expect 201 with a byte-identical
   body, the same project id, and no second analysis. Confirm by watching the API log
   or the analyze counter: the same request must not re-hit GitHub.
6. Re-POST with the same `<KEY>` and a changed field, for example a different name.
   Expect 409 and no new analysis.
7. `curl ... /api/deployments -d '{"service_id":"<service>"}'`, then watch
   `/api/deployments/{id}/logs` (or the UI). Expect the log to show
   `analysing repository...`, then `detected <provider> (<runtime> <version>)`, then
   `building source image...`, and a successful deployment.
8. `docker image inspect <service-image> --format '{{index .Config.Labels "org.opencontainers.image.revision"}}'`
   on the target server (or the equivalent label Railpack sets) matches `<SHA>` for
   `Feature/X`, not `main`.
9. Change the branch head, deploy again, and confirm the resolved SHA and image tag
   move to the new commit. The deployment API still reports the SHA-derived image
   rather than the stored placeholder.

## Failure, cancellation, and retry checks

- **Unbuildable repository:** step 3's analysis against the unbuildable repo returns
  a clear "could not determine how to build" error, and no project is created if the
  failure happens during creation.
- **Build failure:** deploy the unbuildable repo and expect a failed deployment with
  a readable log. No container for the service is left running on the target.
- **Retryable build:** a Railpack exit code 75 is surfaced as a retryable failure
  (`api/jobs/deploy.go` maps that code). Retry the deployment and confirm it either
  succeeds or fails the same way.
- **Lost response:** start the `from-source` request and kill the client before the
  response arrives, then re-send the identical body and key. Expect 201 with the
  original project and exactly one project row.
- **Cancellation/timeout:** start a source deployment and cancel it, and separately
  point a deployment at a repository large enough to hit the deploy timeout. Expect
  the deployment to end in a failed state, the work directory
  `/tmp/stagezero-source-<deploymentID>` to be cleaned up, and no orphaned container.
- **Shared limits:** issue source requests from one user until the rate or
  concurrency limit trips. Expect 429 from both `/api/source/analyze` and
  `/api/projects/from-source`, proving they share one bucket.

## Teardown

1. Delete every service, project, and server created during the run, and confirm the
   target host has no leftover containers and no `/tmp/stagezero-source-*` directories.
2. Drop the disposable database.
3. Revoke the test session tokens and delete the target server's SSH key from both
   StageZero and the host's `authorized_keys`.
4. Delete the test GitHub repositories.
5. Stop and remove the disposable host and any build cache volumes.

## Known gaps this checklist does not close

- There is no automated frontend interaction test in the repository; the browser
  steps above are manual unless a test runner is added.
- The workspace idempotency response snapshot is retained for the life of the
  workspace and can name a project that was later deleted. SZ-09 should confirm that
  a replay of a key whose project was deleted still returns the snapshot rather than
  an error, and decide whether that is acceptable for the release.
- Untrusted Railpack execution still shares a host with the API. That isolation work
  is a separate release gate; SZ-09 must not be run on a production API host until it
  lands.
