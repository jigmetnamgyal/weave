# Code Standards

## General

- Optimize for correctness, security, operability, and clarity before cleverness.
- Keep modules cohesive and single-purpose; do not mix UI, domain, persistence, orchestration, and provider concerns.
- Fix root causes rather than layering workarounds or silent fallbacks.
- Prefer explicit code over hidden framework behavior at security and state-transition boundaries.
- Keep public contracts small, documented, versioned, and backward compatible within a release line.
- All production behavior must be testable without calling paid providers or live customer systems.
- Do not add a dependency until its ownership, security posture, license, update cadence, and operational cost are understood.
- Never log secrets, credentials, raw authorization headers, source code, complete prompts, or unredacted terminal output through infrastructure logs.
- Use UTC for storage and protocols; localize only at display boundaries.
- Represent money and usage with integer minor units, never floating-point values.
- Make retries safe through idempotency and explicit side-effect recording.

## Repository Quality Gates

Every pull request must pass:

- Formatting
- Linting
- Type checking
- Unit tests
- Relevant integration tests
- Build for affected deployable units
- Secret scanning
- Dependency and license scanning
- Static security analysis
- Container vulnerability scan for changed images
- Migration validation when database files change
- OpenAPI compatibility check when API contracts change

No quality gate may be bypassed without a documented, time-bounded exception approved by a maintainer.

## Go

- Use the repository-pinned stable Go toolchain.
- Run `gofmt`, `go vet`, and the configured `golangci-lint` rules.
- Accept `context.Context` as the first parameter for I/O, network, storage, and potentially blocking operations.
- Never store a context in a struct.
- Wrap errors with operational context while preserving errors for `errors.Is` and `errors.As`.
- Use typed domain errors mapped to stable API error codes.
- Avoid panics in request, worker, event, and runner paths; panic is reserved for unrecoverable startup configuration errors.
- Define interfaces at the consumer boundary and keep them narrow.
- Do not create interfaces solely for mocking; use them to express a real boundary.
- Use explicit constructors; validate required dependencies during startup.
- Protect goroutines with cancellation, ownership, bounds, and error propagation.
- Never start an unbounded goroutine per event or connection.
- Use `errgroup` for related concurrent operations.
- Configure all clients with deadlines, connection limits, and retry behavior.
- Use `sqlc`-generated queries and `pgx`; raw dynamic SQL requires review.
- Transactions belong in the application layer and must remain short.
- Temporal workflow code must be deterministic and must not perform direct network, filesystem, clock, or random operations.
- Temporal activities must define start-to-close timeouts, heartbeat behavior where applicable, and retry classification.
- Use table-driven tests for state machines, policies, parsers, and authorization matrices.
- Run the race detector in CI for packages with concurrency-sensitive changes.

## TypeScript

- Strict mode is mandatory, including `noUncheckedIndexedAccess` and `exactOptionalPropertyTypes` unless a documented tool limitation prevents it.
- Do not use `any`. Use `unknown` at untrusted boundaries and narrow it through generated schemas, Zod, or explicit type guards.
- Prefer discriminated unions for session states, event types, async UI states, and provider capabilities.
- Exhaustively handle unions with a `never` assertion.
- Do not duplicate API response types manually; generate them from OpenAPI.
- Avoid non-null assertions except immediately after a checked invariant that TypeScript cannot represent.
- Functions exported across module boundaries require explicit return types.
- Keep browser-only code out of server modules and secrets out of `NEXT_PUBLIC_*` variables.
- Use immutable update patterns for client state.
- Store identifiers as opaque strings; do not infer entity type or authorization from ID format.
- Use `satisfies` for configuration objects where it preserves inference.
- Tests must not rely on wall-clock timing when fake timers or deterministic events are possible.

## Next.js and React

- Default to Server Components for layouts, initial data loading, and non-interactive views.
- Add `"use client"` only at the smallest component boundary requiring browser state or effects.
- Use the generated API client; do not call control-plane endpoints through scattered ad hoc `fetch` helpers.
- Server Actions must not become an alternative unversioned public API. Security-sensitive mutations use the control-plane API.
- Use TanStack Query for mutable server state and explicit invalidation.
- Do not mirror server state into global client state without a demonstrated need.
- Keep components focused on presentation and interaction; business authorization and state transitions remain on the server.
- Effects must synchronize with external systems, not compute derived render state.
- Every effect must have correct dependencies and cleanup.
- Lists require stable domain keys, never array indexes when ordering can change.
- Error boundaries exist around session, diff, terminal, and settings surfaces.
- Suspense and skeletons must preserve layout and avoid large visual shifts.
- Real-time updates merge through an ordered event reducer and never overwrite newer state with stale fetch responses.
- Dangerous controls require clear labels, confirmation appropriate to risk, and disabled/loading states that prevent duplicate submission.

## API Routes

- Validate method, content type, path parameters, query parameters, and body before business logic.
- Authenticate the principal and authorize the exact action against the exact workspace/resource before reading sensitive data or mutating state.
- Never accept `workspace_id`, role, owner, price, quota, or approval authority from the client as trusted facts.
- Return stable JSON error shapes containing `code`, `message`, `request_id`, and optional safe field details.
- Do not expose stack traces, SQL errors, provider credentials, internal hostnames, or raw third-party responses.
- Use cursor pagination for unbounded collections.
- Set explicit size limits for JSON bodies, uploads, event payloads, and logs.
- Mutations that may be retried require an idempotency key and a documented idempotency scope.
- Use optimistic concurrency for mutable resources that can be edited by multiple users.
- Third-party calls use bounded deadlines and execute outside long database transactions.
- Version externally consumed APIs and events.
- Update OpenAPI before or with implementation; generated clients must be committed or reproducibly generated in CI.

## Authentication and Authorization

- Authentication establishes identity; authorization is evaluated separately for every protected operation.
- Centralize permission definitions and deny by default.
- Do not scatter role-name comparisons through handlers or UI components.
- Authorization tests must cover owner, admin, developer, viewer, removed member, wrong workspace, and unauthenticated cases.
- WebSocket subscriptions are authorized at connection and channel-subscription time, and are revoked when access changes.
- Service-to-service calls use workload identity and scoped permissions.
- Avoid confused-deputy behavior: GitHub actions must use the installation associated with the authorized workspace repository.
- Approval eligibility is evaluated when the decision is submitted, not only when the approval request was created.

## Data and Storage

- PostgreSQL owns transactional metadata, product state, permissions, event indexes, and ledgers.
- Object storage owns large logs, reports, patches, snapshots, and artifacts.
- Redis owns disposable presence, rate-limit, and coordination data only.
- NATS transports events but is not the permanent user history.
- Every tenant-owned table includes `workspace_id` and corresponding indexes.
- Repository methods for tenant data require workspace scope explicitly.
- Use UUIDv7 for application-generated primary keys.
- Use `timestamptz` for timestamps and UTC in protocols.
- Use append-only rows for audit, usage, approval decisions, state transitions, and security events.
- Store a cryptographic hash for immutable large artifacts when integrity matters.
- Encrypt provider and integration secrets using envelope encryption; store references rather than plaintext where possible.
- Schema migrations use expand/migrate/contract. Application releases must tolerate the immediately previous compatible schema during rolling deployment.
- Do not delete columns or tighten constraints in the same release that stops writing the old shape.
- Every migration must be tested against representative data volume and have a recovery approach.

## Events and Durable Workflows

- Event names use `<domain>.<entity>.<action>.v<version>` for external transport and a documented normalized type inside session history.
- Every event includes event ID, schema version, occurred time, producer, workspace ID, aggregate ID, and correlation ID.
- Consumers are idempotent and tolerate duplicate delivery.
- Ordering assumptions are limited to a documented aggregate key such as session ID.
- Unknown event fields are ignored; unknown major schema versions are rejected and quarantined.
- Event payloads remain small; large data is stored as an artifact reference.
- Transactional state changes and event publication use an outbox.
- Workflow code must support safe versioning while existing sessions are in flight.
- Retryable and terminal errors are distinct and observable.
- A dead-letter or quarantine path must retain failed event metadata without leaking customer content.

## Runner and Provider Code

- Treat repositories, prompts, model output, dependency scripts, and tool arguments as untrusted input.
- Provider adapters may translate protocol details but may not bypass Weave policy or approval logic.
- Shell commands use argument arrays where possible; never construct a shell string from unescaped user input.
- If a shell is required, commands execute inside the sandbox with explicit working directory, environment allowlist, timeout, and output limit.
- Normalize paths and verify they remain under the session workspace before every filesystem operation.
- Never mount host Docker sockets, host credentials, or broad cloud credentials into a runner.
- Provider capability differences are represented explicitly and tested.
- Strip ANSI control sequences and unsafe terminal escape codes before rendering.
- Bound stdout/stderr and switch to artifact storage after the live limit.
- Redact known and detected secrets before events leave the sandbox.
- Every tool action emits proposed, started, and terminal events with the same action ID.
- Never claim a tool succeeded without an observed exit/result and recorded event.

## Styling

- Use CSS custom-property tokens from `ui-context.md`; no hardcoded product colors in components.
- Use Tailwind semantic utilities mapped to tokens.
- Follow the documented spacing, radius, typography, elevation, and motion scales.
- Support dark and light themes; dark is the default for the developer-first product.
- Preserve information hierarchy with spacing and typography before adding borders or shadows.
- Use color as a supporting signal, never the only signal.
- Avoid decorative gradients in dense work surfaces; reserve the brand gradient for onboarding, marketing, and selected empty states.
- Keep animations subtle and disable non-essential movement under `prefers-reduced-motion`.
- Do not edit generated `components/ui/*` primitives directly unless establishing a documented wrapper pattern.

## Accessibility

- Meet WCAG 2.2 AA for product workflows.
- All interactive elements are keyboard reachable and show a visible focus state.
- Icon-only controls have accessible names and tooltips.
- Dialog focus is trapped and restored correctly.
- Status changes use appropriate live regions without announcing noisy terminal output.
- Terminal and diff views provide text alternatives and do not rely solely on syntax color.
- Touch targets are at least 40 by 40 CSS pixels where layout allows.
- Automated accessibility tests supplement, but do not replace, manual keyboard and screen-reader checks.

## Testing

- Unit tests cover pure domain behavior, policy, validation, reducers, and state transitions.
- Integration tests use real PostgreSQL, Redis, NATS, and Temporal test infrastructure where boundary behavior matters.
- Contract tests verify GitHub, Stripe, identity, and provider adapters against recorded or sandboxed fixtures.
- End-to-end tests cover first-run onboarding, repository connection, shared session, approval, review, pull-request creation, reconnect, and revoked access.
- Security tests cover tenant isolation, broken object authorization, command injection, path traversal, webhook forgery, replay, secret leakage, and unauthorized approvals.
- Failure tests cover duplicate events, delayed events, runner loss, provider timeout, API restart, workflow retry, and GitHub rate limits.
- UI visual regression tests cover major states at supported breakpoints.
- Tests must be deterministic, independently runnable, and clean up their own data.

## File Organization

- `apps/web/` — Next.js application and product UI
- `apps/web/app/` — routes, layouts, loading, and error boundaries
- `apps/web/components/` — feature composition and shared product components
- `apps/web/components/ui/` — generated or wrapped UI primitives
- `apps/web/features/` — feature-level queries, mutations, reducers, and interaction logic
- `services/api/` — Go API entrypoint and transport configuration
- `services/worker/` — Temporal worker entrypoint
- `services/runner-manager/` — isolated environment lifecycle
- `services/runner/` — sandbox-side process supervisor and provider adapters
- `internal/domain/` — pure domain entities, policies, and state machines
- `internal/application/` — use cases, ports, and transaction orchestration
- `internal/adapters/` — infrastructure implementations
- `contracts/openapi/` — API source and generated-client configuration
- `contracts/events/` — event schemas and compatibility fixtures
- `db/migrations/` — ordered SQL migrations
- `db/queries/` — sqlc query definitions
- `infra/` — Terraform, deployment definitions, dashboards, and alerts
- `docs/adr/` — architecture decision records
- `docs/runbooks/` — operational and incident procedures
- `test/fixtures/` — sanitized deterministic fixtures

## Documentation

- Public functions, APIs, events, environment variables, and operational commands must be documented.
- Architecture decisions with lasting tradeoffs require an ADR.
- Runbooks must identify symptoms, diagnostics, mitigation, rollback, and escalation.
- Context documents must be updated in the same change as behavior that invalidates them.
- Never document placeholder behavior as implemented behavior.

