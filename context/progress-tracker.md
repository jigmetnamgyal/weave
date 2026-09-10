# Progress Tracker

Update this file after every meaningful implementation change. It is the concise handoff document for continuing development across sessions. Do not use it as a substitute for detailed issues, ADRs, or runbooks.

## Current Phase

- **Phase 0 — Product and architecture specification**
- Status: Complete

## Current Goal

Establish the production repository foundation and verify the local developer experience before implementing customer-facing features.

## Product Milestones

| Milestone | Outcome | Status |
| --- | --- | --- |
| M0 | Product, UI, architecture, standards, and AI workflow specifications accepted | Complete |
| M1 | Monorepo, local infrastructure, CI, observability bootstrap, and environment validation | Not started |
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

## In Progress

- None.

## Next Up

### Unit M1.1 — Repository Foundation

**Outcome:** A developer can clone the repository, run one documented command, and start the web shell, Go API, PostgreSQL, Redis, NATS, and Temporal development dependencies with health checks.

**Scope:**

- Repository directory structure from `architecture.md`
- Pinned Node, package-manager, Go, and container tool versions
- Next.js application shell with design tokens
- Go API with `/health/live` and `/health/ready`
- Docker Compose for local PostgreSQL, Redis, NATS JetStream, and Temporal
- Base Makefile/task runner commands
- Environment validation and `.env.example` without secrets
- CI jobs for format, lint, type check, unit test, build, secret scan, and dependency scan
- OpenTelemetry bootstrap with local no-op or console-safe exporter

**Non-scope:** authentication, tenant data, GitHub integration, agent providers, runners, billing, and production deployment.

**Acceptance:**

1. Fresh setup succeeds from the documented prerequisites.
2. Web and API health endpoints return successfully.
3. All local dependencies report healthy.
4. CI quality gates pass on an empty feature baseline.
5. No real credential is needed to run the baseline.

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
| 2026-09-10 | Design System and UI Primitives (`context/features-specs/design-system.md`) | Local dev, Node 23, Next.js 16.3.4 | `npx tsc --noEmit`, `npx eslint .`, `npm run build`, manual browser check of Button/Card/Dialog/Input/Tabs/Textarea/ScrollArea at `http://localhost:3000` | Pass | shadcn/ui installed (`style: base-nova`, Base UI primitives, not Radix); Button, Card, Dialog, Input, Tabs, Textarea, ScrollArea added via `npx shadcn add`; `lucide-react` installed; `lib/utils.ts` exports `cn()` from the official `cn` package; `app/globals.css` dark-theme tokens replaced with the hex values from `ui-context.md` (background, foreground, card, popover, primary, secondary, muted, accent, destructive, border, ring, sidebar, chart). Verified visually via a temporary route (removed after verification) — correct dark surfaces/borders, purple primary accent, working dialog with backdrop blur, no hydration errors, no default light styling. |

## Session Notes

- The six context documents are the implementation baseline.
- Build M1.1 before implementing product features.
- Use a fake deterministic agent adapter before integrating a paid provider; this verifies orchestration, events, approvals, and UI independently.
- Do not run untrusted code in the control-plane process or through a host Docker socket.
- Update this file when M1.1 begins, after every meaningful implementation unit, and whenever a blocking decision is discovered.
- Design system components were installed at the repository root (`components/ui/`, `lib/utils.ts`) rather than under `apps/web/` as named in `architecture.md`, because the `apps/web/` monorepo layout has not been created yet (M1.1, Not started). Reversal cost is low: a mechanical path move once M1.1 establishes the monorepo layout.
- `app/globals.css` now carries only the dark-theme token values from `ui-context.md`; light theme and a theme toggle are not implemented. `ui-context.md` requires both themes for the shipped product, so this must be completed before M1.1's "Next.js application shell with design tokens" is considered done, or before any user-facing light/dark toggle ships.
- shadcn's current CLI (`shadcn@4.x`, `style: base-nova`) generates components on Base UI primitives, not Radix, even though `architecture.md` and `ui-context.md` say "Radix primitives." Behavior and accessibility semantics are equivalent for the primitives installed so far; flag this if a future unit relies on Radix-specific APIs.

## Last Updated

- Date: 2026-09-10
- Updated by: Design System and UI Primitives implementation

