# StageZero frontend

SvelteKit, Svelte 5, TypeScript, Vite, and Tailwind CSS v4. The Go API and PostgreSQL setup are documented in the [root README](../README.md).

Install Bun 1.4.3 and Node.js 22, then from this directory:

```sh
cp .env.example .env
bun install --frozen-lockfile
bun run dev
```

Set `API_BASE_URL` in `.env` for server-side API requests; the Vite dev server proxies browser requests under `/api` to `http://localhost:8080`. The API and database must be running for authenticated pages.

```sh
bun run check
bun run lint
bun run build
bun run generate:api
```

`generate:api` writes `src/lib/api/v1.d.ts` from `../api/openapi.yaml`. Commit it alongside spec changes. Bun manages packages and runs scripts, but Vite uses Node.js, and the existing `adapter-auto` does not choose a Bun production server. Configure a supported deployment adapter when deploying.
