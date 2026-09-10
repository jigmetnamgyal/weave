# Weave

Weave is a collaborative workspace where people and coding agents work on real
repositories together: a task becomes a shared session with a plan, live agent
activity, tool approvals, diff review, verification and a pull request.

This repository is at the **foundation** stage. The control plane, session
workflow, providers and runners are specified but not yet implemented. See
[`context/`](context/) for the product, architecture and standards baseline,
and [`context/progress-tracker.md`](context/progress-tracker.md) for what is
built today.

## Prerequisites

Versions are pinned in [`versions.env`](versions.env), which is the single
source of truth and is enforced by `make check-prereqs`.

| Tool           | Version | Notes                                                          |
| -------------- | ------- | -------------------------------------------------------------- |
| Node           | 23.11.0 | Also in `.tool-versions` and `package.json` `engines`          |
| npm            | 10.9.2  | Ships with Node                                                |
| Go             | 1.25.14 | Also in `go.mod`; see the toolchain note below                 |
| Docker         | 28.1.1  | Docker Desktop or an equivalent engine, and it must be running |
| Docker Compose | 2.35.1  | The `docker compose` plugin, not the legacy `docker-compose`   |

If you use [asdf](https://asdf-vm.com) or [mise](https://mise.jdx.dev),
`.tool-versions` covers Node and Go.

**Go toolchain note:** `go.mod` carries two versions. The `go` directive says
`1.25.0` — the minimum language version our dependencies require — while the
`toolchain` directive pins `go1.25.14`, the compiler that actually builds and
tests the module. They differ on purpose: go1.25.0 ships with known
standard-library vulnerabilities that `govulncheck` flags, and 1.25.14 is the
patched release of that line.

Go's default `GOTOOLCHAIN=auto` downloads the pinned toolchain automatically,
so an older base install still builds against the right version — and still
passes `make check-prereqs`, because that check runs `go version` from the
repository root, where the toolchain directive has already been resolved.

## Quick start

```bash
make dev
```

That single command checks prerequisites, creates `.env` from `.env.example`
on first run, installs workspace dependencies, starts the dependency
containers and waits for them to report healthy, then runs the Go API and the
Next.js web shell in the foreground.

| Surface         | URL                                |
| --------------- | ---------------------------------- |
| Web shell       | http://localhost:3000              |
| API liveness    | http://localhost:8080/health/live  |
| API readiness   | http://localhost:8080/health/ready |
| Temporal Web UI | http://localhost:58233             |

Verify the whole stack at any time:

```bash
make health
```

Stop the dependency containers:

```bash
make down
```

No real credential is needed to run this baseline. Every value in
`.env.example` is a local development default.

## Commands

Run `make help` for the full list.

| Command            | Purpose                                                |
| ------------------ | ------------------------------------------------------ |
| `make dev`         | Start dependencies, the API and the web shell          |
| `make up` / `down` | Start / stop just the dependency containers            |
| `make health`      | Report health of every dependency and both API probes  |
| `make logs`        | Follow dependency container logs                       |
| `make clean`       | Stop containers, delete their volumes and build output |
| `make ci`          | Run every quality gate the CI workflow runs            |
| `make fmt`         | Format Go and web sources in place                     |
| `make lint-go`     | golangci-lint at the pinned version                    |
| `make test`        | Go unit tests with the race detector                   |

## Layout

```
apps/web/              Next.js application shell and product UI
  app/                 Routes, layouts, loading and error boundaries
  components/ui/       Generated shadcn/ui primitives — do not edit by hand
  lib/                 Shared web utilities
services/api/          Go control-plane service (health probes today)
services/worker/       Temporal worker entrypoint            (scaffold, M5)
services/runner-manager/  Isolated environment lifecycle     (scaffold, M5)
services/runner/       Sandbox-side supervisor and adapters  (scaffold, M5)
internal/domain/       Pure entities, policies, state machines (scaffold, M2)
internal/application/  Use cases, ports, transactions         (scaffold, M2)
internal/adapters/     Infrastructure implementations         (scaffold, M2)
contracts/openapi/     API contract source                    (placeholder)
contracts/events/      Event schemas                          (placeholder)
db/migrations/         Ordered SQL migrations                 (empty)
infra/                 Local Docker Compose stack
scripts/               Task-runner scripts backing the Makefile
context/               Product, architecture and standards baseline
```

## Local dependencies

`infra/docker-compose.yml` runs PostgreSQL, Redis, NATS JetStream, Temporal
and the Temporal Web UI. Every dependency defines a healthcheck, and
`make up` waits for all of them before returning.

This stack is for local development only. It uses well-known development
credentials, binds to localhost, and offers no durability or security
guarantees. Staging and production are provisioned separately.

### Ports

Host ports are deliberately offset from the defaults these services normally
use, so the stack cannot collide with a PostgreSQL or Redis you already run:

| Service           | Host port | Container port |
| ----------------- | --------- | -------------- |
| PostgreSQL        | 55432     | 5432           |
| Redis             | 56379     | 6379           |
| NATS              | 54222     | 4222           |
| NATS monitoring   | 58222     | 8222           |
| Temporal frontend | 57233     | 7233           |
| Temporal Web UI   | 58233     | 8080           |

This matters more than it looks. A container healthcheck runs _inside_ the
container and passes whether or not the host port actually reaches it, and on
macOS a host process bound to `127.0.0.1` takes precedence over Docker's
`0.0.0.0` publish. Sharing the default ports therefore produces a stack that
reports itself healthy while the application silently reads and writes a
different, pre-existing datastore. Override any port through the matching
variable in `.env`.

## Health probes

The API separates the two probes deliberately:

- `GET /health/live` — is the process running? Touches no dependency, so a
  transient outage never causes an orchestrator to restart a healthy process.
- `GET /health/ready` — can the process serve traffic? Probes PostgreSQL,
  Redis and NATS concurrently and returns `503` if any is unreachable.

Readiness reports only `ok` or `unavailable` per dependency. Connection
detail stays in the service logs, because probe output is unauthenticated and
dependency errors routinely embed hostnames and connection strings.

The API starts even when its dependencies are down — that is exactly the state
readiness exists to report.

## Observability

`OTEL_EXPORTER` selects the trace exporter: `none` (default) installs a no-op
provider, `stdout` pretty-prints spans to the API's stdout. W3C trace context
propagation is registered either way, so incoming trace headers survive this
service even when spans are dropped. A collector endpoint arrives with the
first deployed environment.

## Contributing

Read [`context/ai-workflow-rules.md`](context/ai-workflow-rules.md) before
starting work. In short: read the context documents first, take one bounded
vertical slice, and update `context/progress-tracker.md` when you begin and
when you finish.

Run `make ci` before opening a pull request. It runs formatting, linting
(including golangci-lint), type checks, unit tests and builds — the same gates
as [`.github/workflows/ci.yml`](.github/workflows/ci.yml), which additionally
runs secret scanning, dependency scanning and a live compose health check.

golangci-lint is installed from source at the version pinned in `versions.env`,
never as a prebuilt binary. A golangci-lint release is compiled with whichever
Go its maintainers used, and it refuses to run against a module targeting a
newer Go than that — building it with our own pinned toolchain makes that skew
impossible.
