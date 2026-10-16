# StageZero

A lightweight self-hosted application deployment platform. StageZero allows you to manage projects, services, environments, servers, SSH keys, and custom domains with automated deployment reconciliations.

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

## Quickstart

Install Go 1.25+, Bun 1.4.3, Node.js 22, and Docker with Compose. Bun installs frontend dependencies and runs scripts; Vite and the current SvelteKit adapter still use Node.js. The included Compose file starts PostgreSQL 18 for local development.

### 1. Start Database

Start PostgreSQL via Docker Compose:

```bash
docker compose up -d
```

### 2. Backend Setup (`/api`)

```bash
cd api
cp .env.example .env
# Set ENCRYPTION_KEY in .env (e.g. openssl rand -hex 32).
# DATABASE_URL defaults to the local Compose database.

# Run database migrations
go run ./cmd/migrate

# Start development server
air # or go run .
```

The API server runs at `http://localhost:8080`.

### 3. Frontend Setup (`/frontend`)

```bash
cd frontend
cp .env.example .env
bun install --frozen-lockfile
bun run dev
```

Open `http://localhost:5173` in your browser.

---

## Common Scripts

### Backend (`/api`)
- `make dev`: Run dev server with Air
- `make migrate-up`: Run database migrations
- `make generate`: Run `sqlc` and `oapi-codegen`
- `go build ./...`, `go vet ./...`, `go test ./...`: Backend checks

### Frontend (`/frontend`)
- `bun run dev`: Start Vite development server
- `bun run check`: Run Svelte and TypeScript checks
- `bun run lint`: Run Prettier and ESLint checks
- `bun run build`: Build production bundle
- `bun run format`: Format frontend code
- `bun run generate:api`: Generate TypeScript types from `api/openapi.yaml`

`api/openapi.yaml` is the API contract. After editing it, run `make -C api oapi` and `bun run generate:api` from `frontend/`, then commit the generated files. CI checks for drift. The frontend currently uses SvelteKit's `adapter-auto`; a successful build does not by itself provide a production server. Choose a supported deployment target and adapter before hosting it. Bun does not change the production runtime.

## License

StageZero is licensed under the [Apache License 2.0](LICENSE).
