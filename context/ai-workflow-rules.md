# AI Workflow Rules

## Approach

Build Weave incrementally using a spec-driven, risk-first workflow. These context files define the product boundary, architecture, interaction language, engineering standards, and current implementation state. Read all six context files before starting substantial work and implement against them rather than inventing behavior from the current code alone.

The standard unit of work is a **vertical feature slice** that can be demonstrated and verified end to end. Security-sensitive foundations—tenant isolation, authorization, session state, idempotency, auditability, and runner isolation—must be implemented before features that depend on them.

Do not claim “production ready” based only on a successful build. Production readiness requires tests, observable behavior, secure defaults, migrations, failure handling, runbooks, and verified deployment.

## Source of Truth Order

When sources disagree, use this order:

1. Explicit current user instruction
2. Accepted architecture decision record
3. `architecture.md`
4. `project-overview.md`
5. `ui-context.md`
6. `code-standards.md`
7. `progress-tracker.md`
8. Existing implementation

Do not silently reconcile a meaningful conflict. Record it under Open Questions and request a decision when it changes security, data, cost, compatibility, or user-visible behavior.

## Start-of-Session Checklist

Before editing code:

1. Read `project-overview.md`, `architecture.md`, `code-standards.md`, `ui-context.md`, and `progress-tracker.md`.
2. Inspect the relevant code, tests, migrations, contracts, and recent architecture decisions.
3. Confirm the current goal and choose one bounded vertical slice.
4. Identify affected trust boundaries, permissions, data migrations, events, and external side effects.
5. Define observable acceptance criteria and verification commands.
6. Update `progress-tracker.md` to mark the selected unit In Progress.

## Scoping Rules

- Work on one coherent feature unit at a time.
- Prefer small, reversible, verifiable increments.
- Do not combine unrelated system boundaries in one implementation step.
- Do not refactor unrelated code while delivering a feature.
- Do not add infrastructure merely because it may be useful later.
- Do not weaken an invariant to make implementation easier.
- Keep the application deployable after every merged unit.
- If a change cannot be reviewed and verified end to end in one focused session, split it.

## Required Feature Slice Format

Before implementation, state:

- **Outcome:** what becomes possible for the user or operator
- **Scope:** exact components, routes, services, tables, and events included
- **Non-scope:** nearby behavior intentionally deferred
- **Security:** authentication, authorization, tenant, secret, and runner implications
- **Failure behavior:** expected timeouts, retries, rollback, and user-visible errors
- **Acceptance:** observable conditions that prove completion
- **Verification:** exact tests and commands to run

## When to Split Work

Split an implementation step if it combines:

- UI changes and a new durable workflow that have not been contracted independently
- Multiple unrelated API resources
- Schema redesign and broad feature work
- Authentication/authorization changes and cosmetic refactoring
- Control-plane and runner-plane changes without a versioned contract
- Provider-adapter changes for more than one provider
- A migration that cannot be deployed safely with the previous application version
- Behavior not clearly defined in the context files
- More than one independently reversible product outcome

## Implementation Order

Within a feature slice, use this sequence when applicable:

1. Define or update the domain behavior and acceptance criteria.
2. Update OpenAPI, event schema, or provider contract.
3. Add migration using expand/migrate/contract principles.
4. Implement domain logic and authorization.
5. Implement persistence and asynchronous workflow behavior.
6. Implement UI states and interactions.
7. Add observability and operator diagnostics.
8. Add unit, integration, end-to-end, security, and failure-path tests as appropriate.
9. Validate the complete slice locally.
10. Update context, ADRs, runbooks, and progress tracker.

## Handling Missing Requirements

- Do not invent product behavior that affects permissions, billing, data retention, approvals, or external side effects.
- If a requirement is ambiguous, resolve it in the relevant context file before implementation.
- If the ambiguity does not block safe work, add it to `progress-tracker.md` with the assumption used and its reversal cost.
- If a requirement could cause data loss, security exposure, breaking compatibility, or material cost, stop and request a decision.
- Mark temporary behavior clearly and create a tracked removal condition.
- Never represent a mocked provider, fake integration, or simulated security boundary as production behavior.

## Security Rules

- Treat repository content, task text, uploaded files, provider output, generated commands, and dependencies as untrusted.
- Enforce authentication and authorization server-side at every protected boundary.
- Scope every tenant data query by verified workspace context.
- Never place provider secrets, GitHub credentials, signing keys, or database credentials in browser code, logs, fixtures, or prompts.
- Never run customer code in the API, web, workflow-worker, or developer host process.
- Never bypass approval or runner policy to complete a demonstration.
- Never grant a broader credential when a short-lived scoped credential is possible.
- Add abuse, isolation, and authorization tests when changing a trust boundary.
- Use sanitized synthetic repositories and credentials in tests.

## Data and Migration Rules

- PostgreSQL is the source of truth for durable product state.
- Use transactions for state changes that must commit together.
- Publish asynchronous consequences through the transactional outbox.
- Use idempotency keys for retryable mutations and side effects.
- Migrations follow expand/migrate/contract and must support rolling deployment.
- Never edit an already-applied production migration.
- Destructive schema changes require backup confirmation, measured migration impact, and rollback/recovery documentation.
- Seed data must be synthetic and safe to expose in development.

## API and Event Rules

- Update the contract before or alongside implementation.
- Do not add undocumented response fields, event types, or error codes.
- Consumers must tolerate additive compatible fields.
- Breaking changes require a new API or event version and a migration plan.
- Every runner event is validated, deduplicated, and associated with one session before persistence.
- Large logs and diffs are artifacts; events contain references and summaries.
- Realtime delivery is an optimization over durable history, not the source of truth.

## UI Rules

- Implement every relevant loading, empty, success, permission-denied, offline, reconnecting, and error state.
- Use semantic tokens and component patterns from `ui-context.md`.
- Keep authorization behavior on the server; UI permission checks only improve affordance.
- Never expose raw chain-of-thought. Present concise plans, actions, evidence, results, and uncertainty.
- Preserve user input on recoverable errors.
- Do not auto-scroll users away from content they are reviewing.
- Verify keyboard navigation, focus management, contrast, reduced motion, and responsive behavior.
- For long activity streams and diffs, implement pagination or virtualization before declaring the feature complete.

## Testing Rules

- Add tests at the lowest level that proves the behavior, plus boundary tests for integration risk.
- Test success, permission denial, invalid input, timeout, retry, duplicate delivery, and cancellation where relevant.
- Every security fix includes a regression test.
- Every session state transition includes positive and negative transition tests.
- Every new role permission updates the authorization matrix test.
- External provider behavior uses deterministic fakes or recorded sanitized fixtures in CI.
- Do not make passing tests depend on network access to paid or customer services.
- Do not delete or weaken tests merely to make a build pass without explaining why the prior expectation was invalid.

## Protected Files and Boundaries

Do not modify the following unless the task explicitly requires it:

- `apps/web/components/ui/*` generated primitives; compose wrappers instead
- Applied files in `db/migrations/*`
- Generated API clients and sqlc output; change sources and regenerate
- Lockfiles except when intentionally changing dependencies
- CI security policies, branch-protection assumptions, signing configuration, or infrastructure guardrails
- Provider-adapter contract and event schemas without compatibility review
- Runner sandbox profiles, egress policy, and secret-injection code without a threat-model update
- Production infrastructure state or credentials
- Third-party package internals

## Dependency Rules

- Reuse an approved dependency when it fits the need.
- Record why a new runtime dependency is required.
- Pin versions using the repository package or module system.
- Verify license compatibility and known vulnerabilities.
- Avoid unmaintained packages for authentication, cryptography, sandboxing, or network policy.
- Do not implement custom cryptography.
- Update the SBOM and lockfiles through standard tooling.

## Observability Rules

- Add stable error codes for operationally distinct failures.
- Propagate request, correlation, workflow, and session identifiers.
- Add metrics for new durable jobs, provider calls, and failure modes.
- Exclude user source, prompts, terminal output, secrets, and high-cardinality labels from default telemetry.
- A background feature is incomplete if operators cannot tell whether it is queued, running, waiting, retrying, failed, or completed.

## Keeping Documentation in Sync

Update the relevant context file in the same change whenever implementation changes:

- Product scope or success criteria
- System boundaries or deployment units
- Storage ownership or data relationships
- Authentication, authorization, tenancy, or approval behavior
- Provider capability or adapter contracts
- UI tokens, patterns, or accessibility behavior
- Code conventions or quality gates
- Operational procedures, failure recovery, or service objectives

Create or update an ADR for lasting architectural tradeoffs. Update runbooks when a new alert or operator action is introduced.

## Completion Definition

A feature unit is complete only when:

1. The outcome works end to end within its defined scope.
2. Acceptance criteria are demonstrated or automated.
3. No invariant in `architecture.md` is violated.
4. Authentication, authorization, tenant isolation, and secrets behavior are verified.
5. Failure, retry, cancellation, and reconnect behavior are implemented where applicable.
6. Logs, metrics, traces, and user-visible errors are sufficient to diagnose failures.
7. Database and contract changes are backward-deployable.
8. Tests, linting, formatting, type checks, builds, and security scans pass.
9. Documentation and `progress-tracker.md` reflect the implemented state.
10. No critical or high-severity known vulnerability is introduced.

## Before Moving to the Next Unit

Run the repository-defined equivalents of:

```bash
make format-check
make lint
make typecheck
make test
make test-integration
make build
make security-check
```

Then update `progress-tracker.md` with:

- Completed outcome
- Verification evidence
- Architecture decisions
- Known limitations
- Next smallest feature unit
- Context required to resume

Do not begin the next unit while the current one is red unless the current goal is explicitly to repair that failure.

## AI Output Rules

- Summarize the intended change before editing.
- Cite the files and contracts used to derive behavior.
- Make edits directly when authorized; do not merely describe code that should be written.
- Keep diffs focused and preserve unrelated user changes.
- Report verification results exactly; never state that a command passed if it was not run successfully.
- Distinguish implemented, tested, simulated, deferred, and blocked work.
- End each implementation cycle with the changed files, verification performed, risks, and the next recommended unit.

