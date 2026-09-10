Read `CLAUDE.md` before starting

We're building the repository foundation (Unit M1.1) so a developer
can clone the repo, run one documented command, and start the web
shell, Go API, and local PostgreSQL, Redis, NATS, and Temporal
dependencies with health checks.

This does not include authentication, tenant data, GitHub
integration, agent providers, runners, or billing.

## Repository structure

Set up the directory layout from `architecture.md`:
- `apps/web` — move the existing Next.js app (including the
  `01-design-system.md` work) here
- `services/api` — Go control-plane service entrypoint
- `services/worker`, `services/runner-manager`, `services/runner` —
  empty scaffolds only, no logic yet
- `internal/domain`, `internal/application`, `internal/adapters` —
  empty scaffolds only
- `contracts/openapi`, `contracts/events` — placeholders only
- `db/migrations` — empty, ready for the first migration
- `infra` — local Docker Compose definitions

## Toolchain

- Pin Node, npm, Go, and Docker versions in a committed file.
- Document prerequisites and the one-command setup in `README.md`.

## Web application shell

- Preserve the current dark-theme tokens and shadcn/ui primitives
  when moving the app into `apps/web`.

## Go API

- Scaffold a minimal Go service in `services/api`.
- Implement `GET /health/live` and `GET /health/ready`.
- `/health/ready` checks PostgreSQL, Redis, and NATS connectivity.

## Local infrastructure

- Add a Docker Compose file under `infra/` for PostgreSQL, Redis,
  NATS JetStream, and Temporal, each with a health check.

## Task runner

- Add a Makefile (or equivalent) with at minimum: start the full
  local stack, run health checks, and tear it down.

## Environment validation

- Add `.env.example` covering every local development variable, no
  real secrets.
- Fail fast with a clear error when a required variable is missing.

## CI

- Add CI jobs for format check, lint, type check, unit tests, build,
  secret scanning, and dependency scanning.
- These gates must pass on this empty feature baseline.

## Observability

- Bootstrap OpenTelemetry in the Go API with a local no-op or
  console-safe exporter.

### Check when done

- Fresh clone + one documented command starts web, API, and all
  local dependencies.
- `GET /health/live` and `GET /health/ready` both succeed.
- Docker Compose reports every local dependency healthy.
- CI passes on this baseline.
- No real credential is needed to run the baseline.
