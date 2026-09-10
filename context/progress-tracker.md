# Progress Tracker

Update this file after every meaningful implementation change. It is the concise handoff document for continuing development across sessions. Do not use it as a substitute for detailed issues, ADRs, or runbooks.

## Current Phase

- **Phase 1 — Engineering foundation**
- Status: M1.1 complete; ready for M2 (authentication and tenancy)

## Current Goal

Implement authentication, workspaces, and tenant isolation on top of the verified repository foundation.

## Product Milestones

| Milestone | Outcome | Status |
| --- | --- | --- |
| M0 | Product, UI, architecture, standards, and AI workflow specifications accepted | Complete |
| M1 | Monorepo, local infrastructure, CI, observability bootstrap, and environment validation | Complete |
| M2 | Authentication, workspaces, membership, authorization matrix, and tenant isolation | Not started |
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

## In Progress

- None.

## Next Up

### Unit M2.1 — Authentication Baseline

Deferred until M1.1 is verified. Blocked on Open Question 2 (identity provider). Expected scope: Clerk sign-in with GitHub, server-side session verification behind the OIDC/JWT identity boundary, and one protected route. Workspaces, membership, and the authorization matrix follow as separate units.

## Open Questions

Resolve these before the milestone that depends on them:

1. **Commercial name:** confirm trademark and domain viability for “Weave.” Required before public launch, not before engineering foundation.
2. **Identity provider:** validate Clerk plan limits and enterprise roadmap; retain OIDC/JWT abstraction regardless of decision. Required before M2.
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
| 2026-09-10 | Repository Foundation, Unit M1.1 (`context/features-specs/02-repository-foundation.md`) | Local dev, macOS arm64, Node 23.11.0, Go 1.25.14, Docker 28.1.1, Compose 2.35.1 | `make check-prereqs`; `make ci` (gofmt, prettier, go vet, eslint, tsc, `go test -race`, go build, next build); `golangci-lint run`; `govulncheck`; `npm audit --audit-level=high`; gitleaks via Docker; `make dev` from a clean checkout (no `.env`, no containers, no volumes); `make health`; per-dependency outage probes; `curl` of both probes and the web shell | Pass | Monorepo layout created and the Next.js app moved to `apps/web` via `git mv` (history preserved); dark-theme tokens verified intact in the browser (`--background #090b10`, `--primary #8b7cff`, `--card #131722`, `--ring #a99fff`). Go API serves `/health/live` (200, no dependency I/O) and `/health/ready` (200 when all up; 503 naming the failing dependency, verified by stopping postgres, redis and nats in turn). Compose stack reports all five services healthy. Config validation fails fast listing every missing variable at once. Local gates all green; **the CI workflow file itself is unverified until first push** — each gate's command was verified locally instead. |
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
- Only the directories the M1.1 spec enumerated were created. `docs/adr/`, `docs/runbooks/`, `test/fixtures/` and `apps/web/features/` are named in `code-standards.md` but were deliberately not scaffolded — create each with its first real occupant rather than as an empty placeholder.
- `app/globals.css` now carries only the dark-theme token values from `ui-context.md`; light theme and a theme toggle are not implemented. `ui-context.md` requires both themes for the shipped product, so this must be completed before M1.1's "Next.js application shell with design tokens" is considered done, or before any user-facing light/dark toggle ships.
- shadcn's current CLI (`shadcn@4.x`, `style: base-nova`) generates components on Base UI primitives, not Radix, even though `architecture.md` and `ui-context.md` say "Radix primitives." Behavior and accessibility semantics are equivalent for the primitives installed so far; flag this if a future unit relies on Radix-specific APIs.

## Last Updated

- Date: 2026-09-10
- Updated by: Pull request docstring coverage repair
