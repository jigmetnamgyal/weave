# Progress Tracker

Update this file after every meaningful implementation change. It is the concise handoff document for continuing development across sessions. Do not use it as a substitute for detailed issues, ADRs, or runbooks.

## Current Phase

- **Phase 1 — Engineering foundation**
- Status: M1.1 and M2.1 complete; next is M2.2 (workspaces and membership)

## Current Goal

Implement authentication, workspaces, and tenant isolation on top of the verified repository foundation.

## Product Milestones

| Milestone | Outcome | Status |
| --- | --- | --- |
| M0 | Product, UI, architecture, standards, and AI workflow specifications accepted | Complete |
| M1 | Monorepo, local infrastructure, CI, observability bootstrap, and environment validation | Complete |
| M2 | Authentication, workspaces, membership, authorization matrix, and tenant isolation | In progress |
| M3 | GitHub App installation, repository access, webhook ingestion, and branch operations | Not started |
| M4 | Task model, agent profiles, provider capabilities, and session creation | Not started |
| M5 | Durable session workflow, runner manager, isolated runner, and fake provider adapter | Not started |
| M6 | Claude Code adapter, normalized events, live session room, and reconnect | Not started |
| M7 | Approval policy, tool proxy, diff review, verification, and revision loop | Not started |
| M8 | Codex adapter, commit/pull-request delivery, usage ledger, and quotas | Not started |
| M9 | Billing, production hardening, security review, runbooks, staging, and launch readiness | Not started |

## Completed

- Defined developer-first MVP and long-term multiplayer-agent product boundary.
- Defined modular control plane and isolated execution plane.
- Selected initial technology stack and service boundaries.
- Defined provider-neutral adapter strategy for Claude Code and OpenAI Codex.
- Defined session lifecycle, approvals, tenant model, runner security, and reliability invariants.
- Defined UI design system and flagship shared-session experience.
- Defined repository code standards and AI implementation workflow.
- Installed and configured shadcn/ui design system and UI primitives (see Verification Record).
- Built the repository foundation: monorepo layout, Go API with health probes, local Docker Compose stack, task runner, environment validation, CI quality gates, and OpenTelemetry bootstrap (Unit M1.1 — see Verification Record).
- Built the authentication baseline: database foundation (goose migrations, sqlc, `users` table), the identity-verifier port with a Clerk JWKS adapter, deny-by-default auth middleware, just-in-time user provisioning, `GET /v1/me`, the first OpenAPI document, and Clerk sign-in with a protected route in the web shell (Unit M2.1 — see Verification Record). Verified end to end on 2026-09-10: sign-in with GitHub through a real Clerk application provisions one `users` row and `GET /v1/me` returns it.

## In Progress

### Unit M2.2 — Workspaces and Membership

Started 2026-09-10 on `feat/m2.2-workspaces-membership`, branched from `main` (M1.1 and M2.1 already merged).

**Source:** `context/features-specs/04-workspaces-and-membership.md`

**Outcome:** A signed-in user creates a workspace, becomes its owner, and every permission decision is answered from PostgreSQL against that membership. This is the unit that makes tenant isolation real.

**Scope:** `workspaces`, `workspace_members` and `audit_events` tables; the owner/admin/developer/viewer role model with a centralised action-oriented permission matrix; workspace-scoped authorization after authentication; workspace-scoped data access with no unscoped reads; workspace and member endpoints; the authorization matrix test suite.

**Non-scope, and why:**

- **Invitations** move to M2.3. The original M2.2 sketch included them, but token issue, expiry, revocation, acceptance and the already-a-member and wrong-email edges are their own coherent slice. Until then a workspace has exactly one member, which is enough to scope repositories in M3.
- **Row-level security** is deferred to its own unit and needs the tenancy ADR first. `architecture.md` positions it as defence in depth behind workspace-scoped queries, which this unit establishes; adding it now would mean a non-superuser role, a per-transaction tenant GUC and a migration strategy on top of an already large unit.
- Repositories, sessions and session revocation on membership change follow in M3 and later.

**Note on ordering:** build this before M3. Repository records are workspace-owned, and connecting a GitHub App installation to a workspace that has no membership model would mean retrofitting authorization onto data that already exists.

## Next Up

### Unit M2.3 — Workspace Invitations

Follows M2.2. Expected scope: the `workspace_invitations` table; issuing, revoking and accepting an invitation; token expiry; and the edges that make invitations their own slice — already a member, wrong email, expired, revoked, and re-invited after removal.

## Open Questions

Resolve these before the milestone that depends on them:

1. **Commercial name:** confirm trademark and domain viability for “Weave.” Required before public launch, not before engineering foundation.
2. ~~**Identity provider**~~ — **Resolved 2026-09-10: Clerk**, behind the OIDC/JWT boundary, for authentication only. Clerk Organizations is explicitly not used; workspaces, membership and every permission decision stay in PostgreSQL, because two sources of truth for authorization is how tenant isolation breaks. Chosen over Auth.js because the Next.js/Go split needs a token the Go service can verify via JWKS, and hand-rolling that issuance is custom crypto across a security boundary; chosen over WorkOS because SAML and SCIM are deferred. See ADR-009. **Still outstanding:** confirm current Clerk pricing against projected workspace count — seats are billed per user while Weave bills per workspace, so model the mismatch before launch, not before M2.1.
3. **Runner isolation:** select managed microVM provider versus Kubernetes with gVisor/Firecracker based on team operations capacity, region availability, cold-start target, and cost. Required before M5.
4. **Provider licensing and automation:** confirm current Claude Code and Codex terms, authentication flows, headless interfaces, redistribution constraints, and organization billing. Required before production adapters in M6/M8.
5. **Source retention:** decide whether repository snapshots are retained after session completion or deleted while retaining patches/logs. Required before M5.
6. **Default egress policy:** define minimum domains for language package registries and repository builds, including whether customers configure allowlists. Required before M5.
7. **Approval defaults:** confirm which commands and tools are auto-allowed for the first repository types. Required before M7.
8. **Initial hosting region:** choose based on customer location, isolated-compute availability, provider connectivity, and data policy. Required before staging.
9. **Pricing units:** choose included agent time, provider pass-through, or customer-supplied provider account. Required before M8/M9.

## Architecture Decisions

| ID | Decision | Reason | Status |
| --- | --- | --- | --- |
| ADR-001 | Begin with a modular Go control plane and separate execution plane | Keeps product logic cohesive while isolating untrusted code | Accepted |
| ADR-002 | Use PostgreSQL as product source of truth | Strong transactions, mature operations, and tenant controls | Accepted |
| ADR-003 | Use Temporal for session workflows | Durable timers, signals, retries, cancellation, and recovery for long-running sessions | Accepted |
| ADR-004 | Use NATS JetStream for runner event transport | Durable at-least-once transport and horizontal fan-out | Accepted |
| ADR-005 | Use GitHub App credentials | Repository-scoped, revocable access without long-lived personal tokens | Accepted |
| ADR-006 | Use one ephemeral isolated environment per session | Strong tenant boundary and deterministic cleanup | Accepted |
| ADR-007 | Keep provider behavior behind capability-aware adapters | Avoid product coupling to Claude Code or Codex | Accepted |
| ADR-008 | Bind approvals to immutable proposal hashes | Prevent replay or parameter substitution | Accepted |
| ADR-010 | Enforce tenancy with workspace-scoped queries; defer row-level security | RLS is defence in depth behind scoped queries, and needs a non-superuser role, a per-transaction tenant GUC and pooling care that would make this unit unreviewable. Gated: **RLS ships before the first external customer.** See `docs/adr/0010-tenancy-and-row-level-security.md` | Accepted |
| ADR-009 | Use Clerk for authentication only, never for authorization | The Go API can verify Clerk JWTs via cached JWKS without hand-rolling token issuance across the Next.js/Go boundary; keeping workspaces, membership and permissions in PostgreSQL avoids a second source of truth for tenant isolation and keeps Clerk swappable behind the OIDC/JWT boundary | Accepted |

## Risks

| Risk | Impact | Mitigation | Owner | Status |
| --- | --- | --- | --- | --- |
| Provider CLI terms or interfaces change | Core adapter disruption | Versioned adapters, capability negotiation, contract tests | Unassigned | Open |
| Untrusted repository escapes sandbox | Critical security incident | MicroVM/gVisor isolation, no host socket, egress deny, security testing | Unassigned | Open |
| Runner cost exceeds willingness to pay | Poor unit economics | Usage ledger, quotas, warm-pool measurement, pricing experiments | Unassigned | Open |
| Noisy agent output overwhelms users | Low product usability | Normalized events, summaries, grouping, progressive disclosure | Unassigned | Open |
| Realtime and durable state diverge | Incorrect user decisions | PostgreSQL authority, sequence replay, idempotent reducer | Unassigned | Open |
| Broad product scope delays launch | No customer validation | Enforce developer-only MVP and milestone gates | Unassigned | Open |

## Verification Record

| Date | Unit | Environment | Commands/tests | Result | Notes |
| --- | --- | --- | --- | --- | --- |
| 2026-09-10 | Workspaces and Membership, Unit M2.2 (`context/features-specs/04-workspaces-and-membership.md`) | Local dev, macOS arm64, Go 1.26.8, PostgreSQL 17.2 | `make ci`; `make test-integration`; `make migrate-up` / `migrate-down` / `migrate-up`; `psql` probes of the append-only triggers; `curl` of all seven routes unauthenticated; browser check of the signed-out redirect | Pass, with one gap | Authorization matrix pinned by an exhaustiveness test that fails the build when a role or permission is added without deciding every pairing, plus a hand-written expectation table so the matrix cannot be changed without changing the test. HTTP tests cover every workspace-scoped endpoint × every role: a non-member receives 404 on all five (never 403), an unknown workspace is byte-identical to one that is not yours, and a member lacking a permission receives 403. Integration tests against real PostgreSQL prove cross-tenant reads return nothing, the last owner cannot be demoted or removed, two concurrent owner demotions leave exactly one owner (the reason the store takes a row lock), a stale version is rejected, refused changes write no audit row, and audit rows survive deleting the workspace they describe. Append-only is enforced by the database: UPDATE, DELETE and TRUNCATE are all rejected by trigger. All seven routes return 401 unauthenticated while `/health/*` stays public. **Gap: the signed-in browser flow is unverified** — it needs a GitHub sign-in only the operator can perform. |
| 2026-09-10 | Authentication Baseline, Unit M2.1 (`context/features-specs/03-authentication-baseline.md`) | Local dev, macOS arm64, Go 1.25.14, Node 23.11.0, PostgreSQL 17.2 | `make ci` (now including `sqlc-check` and `lint-go`); `make test-integration` against the compose database; `make migrate-up`; `psql \d users`; `curl` against a running API for every auth path; browser check of the web shell | Pass, with one gap | Migration applied and schema matches the spec. Integration tests against real PostgreSQL prove 16 concurrent first requests create exactly one row, that citext makes email lookup case-insensitive, and that empty profile fields store as NULL. Verifier tests use a locally generated RSA key and an httptest JWKS server — no Clerk network call — covering valid, expired, wrong issuer, wrong audience, unpublished key, tampered payload, malformed, empty, missing-email, and HMAC algorithm confusion. Live API: `/health/*` public and 200; `/v1/me` returns 401 with the standard envelope and an echoed `X-Request-Id` for missing, malformed and wrong-scheme credentials; a well-formed RS256 token against an unreachable issuer returns 503, while `alg:none` returns 401 without any network call. Gap subsequently closed: a real Clerk application was linked, `session.claims` configured, and sign-in with GitHub confirmed to create exactly one `users` row carrying the real email, with `GET /v1/me` returning it. |
| 2026-09-10 | Repository Foundation, Unit M1.1 (`context/features-specs/02-repository-foundation.md`) | Local dev, macOS arm64, Node 23.11.0, Go 1.25.14, Docker 28.1.1, Compose 2.35.1 | `make check-prereqs`; `make ci` (gofmt, prettier, go vet, eslint, tsc, `go test -race`, go build, next build); `golangci-lint run`; `govulncheck`; `npm audit --audit-level=high`; gitleaks via Docker; `make dev` from a clean checkout (no `.env`, no containers, no volumes); `make health`; per-dependency outage probes; `curl` of both probes and the web shell | Pass | Monorepo layout created and the Next.js app moved to `apps/web` via `git mv` (history preserved); dark-theme tokens verified intact in the browser (`--background #090b10`, `--primary #8b7cff`, `--card #131722`, `--ring #a99fff`). Go API serves `/health/live` (200, no dependency I/O) and `/health/ready` (200 when all up; 503 naming the failing dependency, verified by stopping postgres, redis and nats in turn). Compose stack reports all five services healthy. Config validation fails fast listing every missing variable at once. Local gates all green; **the CI workflow file itself is unverified until first push** — each gate's command was verified locally instead. |
| 2026-09-10 | M1.1 review follow-up (CodeRabbit on PR #1) | Local dev, macOS arm64, Docker 28.1.1 | `make ci` (gofmt, prettier, go vet, eslint, tsc, `go test -race`, go build, next build); `golangci-lint run` (0 issues); `govulncheck` at the newly pinned v1.7.0; gitleaks v8.30.1 in git mode; full compose stack up with `--wait`; both probes via `curl` and `scripts/health.sh` | Pass | Nine inline findings triaged: eight fixed, one rejected on evidence. Two fixes were measured rather than assumed — the NATS readiness probe returned in 10.01s before the change and 3.01s (the configured `ReadinessTimeout`) after, with the NATS container paused to simulate an open-but-unresponsive server; and `scripts/health.sh` was reproduced building `http://localhost127.0.0.1:8080` from a host-qualified `API_HTTP_ADDR` before the fix. Compose publishers confirmed bound to 127.0.0.1 only. |
| 2026-09-10 | Design System and UI Primitives (`context/features-specs/design-system.md`) | Local dev, Node 23, Next.js 16.3.4 | `npx tsc --noEmit`, `npx eslint .`, `npm run build`, manual browser check of Button/Card/Dialog/Input/Tabs/Textarea/ScrollArea at `http://localhost:3000` | Pass | shadcn/ui installed (`style: base-nova`, Base UI primitives, not Radix); Button, Card, Dialog, Input, Tabs, Textarea, ScrollArea added via `npx shadcn add`; `lucide-react` installed; `lib/utils.ts` exports `cn()` from the official `cn` package; `app/globals.css` dark-theme tokens replaced with the hex values from `ui-context.md` (background, foreground, card, popover, primary, secondary, muted, accent, destructive, border, ring, sidebar, chart). Verified visually via a temporary route (removed after verification) — correct dark surfaces/borders, purple primary accent, working dialog with backdrop blur, no hydration errors, no default light styling. |
| 2026-09-10 | Pull request docstring coverage repair | Local sandbox, Node 24.14.1, Go 1.25.14 | `make fmt-check lint typecheck test` | Pass | Documented all 52 top-level functions in the M1.1 pull-request diff. The external docstring coverage gate remains pending its next pull-request run. |

## Session Notes

- The six context documents are the implementation baseline.
- Build M1.1 before implementing product features.
- Use a fake deterministic agent adapter before integrating a paid provider; this verifies orchestration, events, approvals, and UI independently.
- Do not run untrusted code in the control-plane process or through a host Docker socket.
- Update this file when M1.1 begins, after every meaningful implementation unit, and whenever a blocking decision is discovered.
- Design system components have been moved from the repository root to `apps/web/` as part of M1.1, resolving the temporary placement noted during the design-system unit.
- **Local dependency ports are offset from their defaults** (PostgreSQL 55432, Redis 56379, NATS 54222/58222, Temporal 57233/58233). This was found the hard way: a host Redis already owned 6379, the compose stack still reported healthy because container healthchecks run inside the container, and on macOS a host process bound to `127.0.0.1` wins over Docker's `0.0.0.0` publish — so readiness passed while the API silently talked to the wrong datastore. Do not "simplify" these back to the standard ports.
- **`go.mod` carries both a `go` directive (1.25.0) and a `toolchain` directive (go1.25.14).** They differ deliberately: 1.25.0 is the minimum language version the dependencies require, and go1.25.0 ships standard-library CVEs that `govulncheck` flags. `golang.org/x/text` was also bumped to v0.39.0 for the same reason. `govulncheck` is clean; re-check it when bumping either.
- Go commands use explicit package selectors (`./services/... ./internal/...`) rather than `./...`, because npm workspaces hoist `node_modules` to the repository root and one transitive npm package vendors a `.go` file that would otherwise be treated as part of this module.
- The web workspace has **no unit tests**; the `test` gate covers Go only. A web test runner should be added with the first stateful UI logic (reducers, event merging), not before.
- `/health/ready` probes PostgreSQL, Redis and NATS but **not Temporal**, matching the spec exactly. Temporal runs in the local stack and its host/port is validated config. Add a Temporal probe when the session workflow in M5 makes it a hard serving dependency.
- Readiness responses report only `ok`/`unavailable` per dependency; the underlying error is logged, never returned, because probe output is unauthenticated and dependency errors embed hostnames and connection strings.
- **The authorization matrix in `internal/domain/authorization.go` is the only place a role implies anything.** Nothing else compares a role name. `TestMatrixIsExhaustive` fails the build if a new role or permission is added without deciding every pairing — including the denials, written out as `false`, because an omission denies at runtime but records no decision.
- **Non-member is 404, member-without-permission is 403.** A 403 for a workspace that exists but is not yours confirms its existence and turns identifier guessing into tenant enumeration. A test asserts the two responses are indistinguishable; do not "improve" the non-member case to 403.
- **The last-owner rule is decided under `SELECT ... FOR UPDATE` on the workspace row.** Without the lock two concurrent demotions each observe two owners and each proceed, leaving none — a workspace nobody can administer. There is a test for exactly that race.
- **`audit_events` has no foreign keys and two triggers.** No FK so records outlive the workspace they describe; a row trigger blocking UPDATE and DELETE, and a statement trigger blocking TRUNCATE, because a row trigger alone leaves the table erasable in one statement.
- **`make ci` fails locally on `sqlc-check` until generated code is staged.** The gate runs `git diff --exit-code` against the working tree, which is correct in CI where everything is committed. Run `git add` first, or expect one confusing failure.
- **A Clerk instance needs `infra/clerk/session-claims.json` applied before sign-in works.** Clerk's default session token carries no email, so the API rejects an otherwise-valid credential with `token carries no email claim` — provisioning cannot create a user from it. Apply with `clerk config patch --file infra/clerk/session-claims.json`. The alternatives were a Clerk secret key in the Go API for Backend API lookups, or nullable emails; adding the claim keeps the secret out of the API entirely. `CLERK_ISSUER` is not written by the CLI (it is ours) and is derived from the publishable key, which encodes the frontend domain. Google sign-in is still enabled on the dev instance and the spec says GitHub only — disable with `clerk config patch --json '{"connection_oauth_google":{"enabled":false}}'`.
- **`make dev` builds the API to `bin/api` and runs it directly, never via `go run`.** `go run` compiles to a temporary binary and execs it as a child, so the PID it reports is the wrapper: killing it leaves the server running, reparented to init, still holding port 8080, and the next `make dev` fails with a bind error that looks unrelated. `dev.sh` also enables job control and signals process groups, because `npm run dev` is three processes deep (npm, next, next-server) and signalling only the wrapper orphans the server on port 3000. It refuses to start at all when either port is occupied, naming the process and the kill command.
- **`apps/web/.env` is a symlink to the root `.env`, created by `make dev` / the Makefile's `.env` target.** Next.js reads env files only from its own directory, and neither exporting the variables into the process nor calling `loadEnvConfig` from `next.config.ts` reaches the edge runtime that `proxy.ts` runs in or the `NEXT_PUBLIC_` inlining the client bundle needs. Both were tried and both failed; the symlink is what works. It is gitignored, so a fresh clone gets it from the task runner.
- **The migration runner is `services/migrate`, not the goose CLI.** Installing the CLI links every database driver goose supports (ClickHouse, YDB, libsql, MySQL); it filled the disk mid-build. Driving goose as a library reuses the pgx driver already present. Migrations run as their own step, never at API startup.
- **Web route protection lives in `app/(app)/layout.tsx`, not in path patterns.** Clerk deprecated `createRouteMatcher` because path matching can diverge from how Next.js resolves routes and leave protected resources reachable. The layout protects everything nested in the route group, mirroring how the API wraps its whole `/v1` subtree — in both cases a new route is protected by where it sits, not by remembering.
- **Clerk Core 3 removed `<SignedIn>` / `<SignedOut>`;** use `<Show when="signed-in">`. The old names are still exported and throw at runtime, so this fails at render rather than at compile.
- Go commands and gates now include `sqlc-check`, which regenerates and fails on a diff. A Homebrew-installed sqlc shadowed the pinned one during this unit and generated output at the wrong version, which is exactly what that gate catches.
- Only the directories the M1.1 spec enumerated were created. `docs/adr/`, `docs/runbooks/`, `test/fixtures/` and `apps/web/features/` are named in `code-standards.md` but were deliberately not scaffolded — create each with its first real occupant rather than as an empty placeholder.
- `app/globals.css` now carries only the dark-theme token values from `ui-context.md`; light theme and a theme toggle are not implemented. `ui-context.md` requires both themes for the shipped product, so this must be completed before M1.1's "Next.js application shell with design tokens" is considered done, or before any user-facing light/dark toggle ships.
- **`natsProbe` uses `conn.FlushWithContext(ctx)`, never `conn.RTT()`.** `RTT()` flushes with a hardcoded 10s timeout that ignores the caller's context, and `health.Ready` waits for every probe, so one unresponsive NATS server held the whole readiness response past `ReadinessTimeout`. Measured: 10.01s before, 3.01s after.
- **Every Compose port is published on `127.0.0.1`, not the Compose default of all interfaces.** The stack uses development credentials or none; an unqualified publish bypasses the host firewall. This is separate from, and additional to, the port offsets noted above.
- **The check-prereqs Go check measures the *resolved* toolchain, not the base install.** `scripts/check-prereqs.sh` runs `go version` from the repository root, where `GOTOOLCHAIN=auto` has already applied go.mod's `toolchain go1.25.14`, so a developer whose base Go is older still passes and still builds against the pin. Verified on a machine whose base Go is 1.23.5. A review comment asserted the opposite (that the prereq gate blocks the `GOTOOLCHAIN=auto` fallback) and was rejected on this evidence — do not "fix" the docs to match that claim without re-testing.
- **`apps/web/components/ui/tabs.tsx` carries one deliberate deviation from generated shadcn output:** `orientation` is forwarded to `TabsPrimitive.Root`, not just rendered as `data-orientation`. Upstream swallows it, so vertical tabs got correct styling but horizontal arrow-key navigation and `aria-orientation`. The file comments the deviation; re-apply it if the component is regenerated.
- **`versions.env` pins the security scanners** (`GOVULNCHECK_VERSION`, `GITLEAKS_VERSION`) so a gate result is reproducible. govulncheck is held at 1.7.0 rather than 1.8.0 because 1.8.0 requires go1.26 and would make CI download a second toolchain purely to build the scanner.
- **`.gitleaks.toml` allowlist regexes match exact synthetic values only.** Note that the committed development credentials are low-entropy enough that gitleaks' default ruleset does not flag them at all — verified by scanning with the allowlist block removed, which still reports no leaks. The allowlist is defence-in-depth against a future rule change, not currently load-bearing.
- shadcn's current CLI (`shadcn@4.x`, `style: base-nova`) generates components on Base UI primitives, not Radix, even though `architecture.md` and `ui-context.md` say "Radix primitives." Behavior and accessibility semantics are equivalent for the primitives installed so far; flag this if a future unit relies on Radix-specific APIs.

## Last Updated

- Date: 2026-09-10
- Updated by: Workspaces and Membership (Unit M2.2) implementation
