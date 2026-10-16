# Architecture decisions

Existing choices visible in code: OpenAPI contract with generated Go/TypeScript bindings (`api/openapi.yaml`, `api/gen`, `frontend/src/lib/api/v1.d.ts`); workspace-scoped PostgreSQL data and Goose migrations (`api/db`); in-process deployment jobs with startup reconciliation (`api/jobs`); SSH-based remote Docker and Traefik management (`api/ssh`, `api/proxy`). These are descriptions, not new approved ADRs.

Decisions still needed before production: SSH host-key trust, frontend hosting adapter/reverse proxy, and migration rehearsal for existing data. The [roadmap](../../roadmap.md) records proposals and their verification gates; no replacement architecture has been adopted by this documentation change.
