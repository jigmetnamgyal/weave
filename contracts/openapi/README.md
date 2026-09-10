# OpenAPI contracts

Canonical description of the Weave control-plane HTTP API.

Placeholder: no contract is defined yet. The API currently exposes only the
operational probes `GET /health/live` and `GET /health/ready`, which are not
part of the versioned public API surface.

## Rules

- The contract is updated **before or with** the implementation it describes.
- The TypeScript client is generated from this source; it is never hand-written
  or duplicated in `apps/web`.
- Externally consumed APIs are versioned. Breaking changes require a new
  version and a migration plan.
- Additive, compatible fields must not break existing consumers.

See `context/code-standards.md` (API Routes) and `context/architecture.md`.
