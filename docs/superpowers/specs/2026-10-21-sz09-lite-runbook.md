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
     build needs about ten minutes and several gigabytes of disk.
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
  per run. The API log is redacted before it is uploaded as an artifact.
- The analysis rate limit is raised for the test; one concurrent analysis is
  kept on purpose, so an analysis slot leaked by a cancelled request shows up
  as a `429` in the follow-up check.

## Failure diagnostics

A failed run uploads the redacted API log as the `sz09-lite-diagnostics`
artifact. The test output names the failed check and the API response that
contradicted it.
