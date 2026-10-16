# StageZero

A lightweight self-hosted application deployment platform. StageZero allows you to manage projects, services, environments, servers, SSH keys, and custom domains with automated deployment reconciliations.

Engineering documentation: [architecture, development, operations, feature audits, and roadmap](docs/README.md). The audit describes work on feature branches, not capabilities already shipped on `main`.

## Features

- Project & Service Management: Organize applications and databases into projects.
- Multi-Environment Support: Configure Development, Staging, and Production environments.
- Server & SSH Access: Manage remote servers via SSH keys for deployments.
- Domain & Ingress Management: Automatic domain reconciliation and SSL/ingress handling.
- Authentication & Security: Cookie-based sessions, GitHub/Google OAuth2, and AES-256 encrypted secrets at rest.
- Modern Dashboard: Built with Svelte 5 runes, Tailwind CSS v4, and responsive inspector side panels.

## Tech Stack

### Backend (`/api`)
- Go 1.25+
- `net/http` standard library with `oapi-codegen` OpenAPI 3 router
- PostgreSQL 18 with `pgx/v5` and `sqlc`
- Database migrations with `goose`
- Hot reload with `air`

### Frontend (`/frontend`)
- SvelteKit (Svelte 5)
- Tailwind CSS v4, Bits UI, Lucide Icons
- TypeScript, Vite, Bun (package manager and development script runner)

---

## Local development

Prerequisites: Bun 1.4.3, Go 1.25+, Node.js 22+, Docker Engine 24+ with Compose v2+ and a running local daemon. Bun manages the JavaScript workspace; Node.js runs Vite. Docker Compose provides PostgreSQL 18. Install these tools yourself before setup; setup never installs system software or invokes `scripts/install.sh` (the separate production server bootstrap).

```bash
git clone https://github.com/centrys-labs/stagezero.git
cd stagezero
bun run setup
bun run dev
```

Open `http://localhost:5173` (API: `http://localhost:8080`). `setup` installs locked Bun/Go dependencies, creates missing `api/.env` and `frontend/.env`, generates a 32-byte hex development encryption key, starts only Compose's PostgreSQL service, waits for health, and applies migrations. It preserves existing environment files, keys, and database volumes. Run it again safely after an interruption. Use `bun run setup --skip-db` when Docker is unavailable, or `--skip-install` to reuse installed dependencies. `bun run setup --help` lists options. `bun run dev:api` uses Air when installed for Go hot reload; otherwise it uses `go run .` without hot reload. `bun run dev:web` runs Vite with hot reload.

| Command | Purpose |
| --- | --- |
| `bun run dev` / `dev:api` / `dev:web` | Both development servers / individual server |
| `bun run build` / `build:api` / `build:web` | Build both / Go / web (Go emits no binary into the repo) |
| `bun run check` / `lint` / `format` / `test` / `test:api` | Go vet and Svelte checks / Prettier + ESLint / web formatting / Go and setup tests / Go tests |
| `bun run db:up` / `db:down` / `db:migrate` | Start local PostgreSQL / stop without deleting data / guarded local migration |
| `bun run generate` / `generate:api` | SQLC + Go OpenAPI + TypeScript OpenAPI / TypeScript only |

`generate` requires `sqlc` v1.30.0 and `oapi-codegen` v2.6.0 on PATH (install with `go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.30.0` and `go install github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.6.0`). CI checks generated files for drift. `api/openapi.yaml` is the shared API contract. `go test -race ./...` from `api/` is also run in CI.

`api/.env` needs `DATABASE_URL` (the local Compose URL is in the example) and `ENCRYPTION_KEY` (auto-generated only for new files). OAuth credentials are optional but required to use GitHub/Google sign-in; add the relevant ID and secret pairs in `api/.env` yourself. `frontend/.env` contains `API_BASE_URL=http://localhost:8080` for server-side requests; Vite proxies browser `/api` calls to the same API. Migrations refuse URLs other than the expected local Compose credentials/host/port/database and require a healthy local Compose container. If you use another database, manage its migrations explicitly outside these root commands.

If setup cannot reach Docker, start the daemon and check your user has access. If port 5432 is occupied, stop the conflicting service rather than resetting the Compose volume. If the database rejects local credentials, an existing Compose volume may have been initialized for another user; inspect it and back up data before making any manual changes. Setup will not reset it. If migrations are interrupted, rerun setup after verifying health (`docker compose ps postgres`). For missing Bun/Go/Node, install the indicated version and rerun. If the web UI cannot reach the API, check both servers and `frontend/.env`'s `API_BASE_URL`. The frontend uses SvelteKit `adapter-auto`; a successful build does not provide a production server without an appropriate deployment adapter. Bun does not change production hosting.

## License

StageZero is licensed under the [Apache License 2.0](LICENSE).
