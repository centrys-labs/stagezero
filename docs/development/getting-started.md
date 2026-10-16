# Local development

These instructions describe `origin/main` at `0e3c0ab`. The root [README](../../README.md) is the short-form setup; this page explains the boundaries and failure cases. No production infrastructure is created by these commands.

## Prerequisites and first run

- Bun 1.4.3, Go 1.25+, Node.js 22+, Docker Engine 24+ and Docker Compose v2+; Docker must be running and accessible to your user.
- Ports 5432 (PostgreSQL), 8080 (Go API) and 5173 (Vite) available.
- For actual deployments: a separate reachable Linux server with Docker, SSH key access, DNS pointing to it for domains, and outbound image registry access. A local DB alone cannot exercise deployment.

From the repository root:

```bash
bun run setup
bun run dev
```

Open `http://localhost:5173`; the API listens on `http://localhost:8080`. `scripts/setup.ts` installs locked Bun packages and Go modules, creates missing `api/.env` and `frontend/.env` (generating a 32-byte encryption key only for a new API env file), starts the **PostgreSQL-only** `compose.yaml` service and applies embedded Goose migrations. It preserves existing env files, encryption keys and database volumes. `bun run setup --help` documents `--skip-db`, `--skip-install` and the migration-only mode. Do not delete or regenerate `ENCRYPTION_KEY` for an existing database: stored SSH keys and environment secrets depend on it.

`bun run dev:api` runs Air if installed, otherwise `go run .`; `bun run dev:web` runs Vite. Browser `/api` traffic is proxied to the API by `frontend/vite.config.ts`; server-side SvelteKit loads use `API_BASE_URL` from `frontend/.env`. `COOKIE_SECURE=false` in `api/.env.example` is for HTTP localhost only; use secure cookies behind HTTPS in deployment. OAuth login works only after configuring the appropriate `GITHUB_CLIENT_ID`/`GITHUB_CLIENT_SECRET` or Google pair and a matching `OAUTH_REDIRECT_BASE_URL`. GitHub login credentials are **not** repository-import credentials.

## Working on contracts and schema

Edit `api/openapi.yaml` first for HTTP interfaces and `api/db/queries/` plus migrations for persisted data. Run `bun run generate` to regenerate `api/gen/api.gen.go`, `api/db/sqlcgen/` and `frontend/src/lib/api/v1.d.ts`; this needs `sqlc` v1.30.0 and `oapi-codegen` v2.6.0 on PATH (see [README](../../README.md#local-development)). Commit generated changes together with their sources. CI checks for drift. `api/db/migrate.go` embeds migrations into the API binary; `api/cmd/migrate/main.go` is the raw up/down command. `bun run db:migrate` runs a local database identity guard in `scripts/setup.ts`; the raw Go migration command does **not** have that guard. Back up and validate data before nonlocal migrations; avoid running `down` as a recovery plan for deployments or secrets.

## Troubleshooting without destructive resets

- No Docker access: start the daemon, confirm user access, then rerun setup. `bun run setup --skip-db` prepares dependencies and env files when Docker is unavailable but cannot make API persistence work.
- Port 5432 in use or credentials rejected: inspect `docker compose ps postgres` and the existing Compose volume/DB identity; do not delete the volume to make the warning disappear.
- API starts but login/requests fail: check `DATABASE_URL`, migrations, both dev processes, `frontend/.env`'s `API_BASE_URL`, and browser proxy configuration. OAuth requires provider registration matching the callback URL.
- Server registration/deploy fails: check server-side Docker permissions (direct or noninteractive `sudo -n docker`), SSH reachability, registry access and Traefik/DNS rather than resetting the local database.
- Frontend build warns that adapter-auto cannot identify an environment: see [deployment limitations](../operations/deployment.md); a successful build is not evidence of a runnable production frontend.

Use [testing](testing.md) before opening a PR. Source builds and monitoring have additional, **branch-only** requirements in [GitHub import](../features/github-import.md) and [metrics](../features/metrics.md).
