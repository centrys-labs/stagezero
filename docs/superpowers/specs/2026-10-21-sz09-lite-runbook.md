# SZ-09 Lite: disposable smoke test on GitHub-hosted runners

A functional smoke test of the combined candidate that runs entirely on a
GitHub-hosted Ubuntu runner. It uses a PostgreSQL 18 service container, the
combined API binary, the pinned Railpack release, and one small trusted public
GitHub repository. It never touches a self-hosted runner, a production host, a
production database, or the operator's VPS.

This is a runbook, not a design document. The wider SZ-09 procedure and its
authorization boundary stay in
`docs/superpowers/specs/2026-10-17-sz09-import-railpack-verification.md`.

## What the runner covers

| Check | Where it runs |
| --- | --- |
| Real source analysis of a public fixture | GitHub-hosted runner |
| Branch and commit resolution against the remote | GitHub-hosted runner |
| Atomic project creation | GitHub-hosted runner |
| Byte-exact idempotency replay and changed-payload conflict | GitHub-hosted runner |
| Analysis errors (unknown branch, unknown repository, foreign host) | GitHub-hosted runner |
| Cancellation and cleanup of an interrupted analysis | GitHub-hosted runner |
| Railpack preparation of the fixture | GitHub-hosted runner |
| BuildKit build of the fixture with the pinned frontend | GitHub-hosted runner, opt-in |

Still not covered, because they need a separate disposable target host and
separate authorization: SSH deployment, deployment log streaming, cancelling a
running deployment, rollback, and cleanup of a deployed service. Untrusted
repository execution is a separate P0 release gate and is not exercised here.

## Operator setup

1. Create a repository label named `sz09-lite`.
2. Set repository variables:
   - `SZ09_FIXTURE_REPO`: `owner/name` of a small public repository the
     operator controls. Required.
   - `SZ09_FIXTURE_BRANCH`: the branch to import. Defaults to `main`.
   - `SZ09_FIXTURE_SHA`: the commit the import has to resolve to. Optional;
     without it the test reads the remote ref directly.
   - `SZ09_LITE_BUILD`: set to `1` to include the BuildKit build. Optional; the
     build needs about ten minutes and 9 GB of free disk on the runner.
3. Open a pull request from a branch of this repository into
   `integration/sz11b-candidate`.
4. Add the `sz09-lite` label. The workflow starts. Pull requests from forks and
   pull requests with any other base branch are refused by the job condition.
5. Remove the label to stop further runs. Adding it again starts a new run.

`workflow_dispatch` is listed in the workflow file, but GitHub only offers it
once the file exists on the default branch. Until the workflow reaches `main`,
the label is the only trigger.

## Fixture requirements

A small public repository the operator controls, with a build the pinned
Railpack release understands (`package.json`, `go.mod`, or similar), a `main`
branch, and nothing secret in it. Keep it small: the analysis downloads a
tarball of it and the build installs its dependencies.

## Boundaries inside the run

- The runner is ephemeral and has no inbound route. The API binds port 8080 on
  the runner only, and no deployment port is opened anywhere.
- No repository secret is referenced. `GITHUB_TOKEN` has `contents: read`.
- The encryption key, the session token, and the database password are created
  per run. The API log is redacted before it is uploaded as an artifact, and
  the redaction holds back the tail of the stream, so a secret that is split
  across two writes is redacted as well.
- The analysis rate limit is raised for the test; one concurrent analysis is
  kept on purpose, so an analysis slot leaked by a cancelled request shows up
  as a `429` in the follow-up check.

## Capacity policy

The job measures the free space of the filesystem that holds the Docker data
root, the workspace and the runner temporary directory before it starts, and
refuses to run when it is too small:

| Run | Free disk required | Measured peak |
| --- | --- | --- |
| API only | 2 GB | about 1 GB (Go modules 64 MB, Go build cache 178 MB, the api binaries 80 MB, the postgres:18-alpine image 433 MB, railpack 40 MB, the fixture tree) |
| With the BuildKit build | 9 GB | about 4.5 GB (the buildkit image 365 MB, the railpack frontend 46 MB, the railpack builder image 1.6 GB, the node runtime, the npm cache, the exported image layers) |

The requirement is twice the measured peak and stays inside the 14 GB of the
standard runner, so a build that cannot fit is refused before it starts instead
of running out of disk halfway through. The runner is never cleaned up
destructively and the run never moves to a self-hosted runner. When the build
does not fit, unset `SZ09_LITE_BUILD` and run the API only smoke.

## Timeouts

- The BuildKit build runs under a 25 minute deadline. The docker client is
  started with that context, so a build that overruns is killed with its
  process group, the BuildKit daemon cancels the solve when the client
  disappears, and the disposable image is removed.
- The whole job is limited to 45 minutes and the Go test to 30 minutes.
- The cancellation check cancels the analysis while the server is working on
  it, observed through the temporary source tree, so it does not depend on how
  fast the runner is.

## Failure diagnostics

A failed run uploads the redacted API log as the `sz09-lite-diagnostics`
artifact. The test output names the failed check and the API response that
contradicted it.
