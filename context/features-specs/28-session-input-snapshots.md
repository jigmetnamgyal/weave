# M6.1b.2a — Immutable session inputs

**Status: built for review on `m6.1b2-session-inputs`; full workflow integration is blocked by local Temporal availability.** Credential/runtime activation remains disabled.

## Outcome

A session's task title/body is captured atomically at creation, not reread from
an editable task when its runner eventually starts. Agent settings remain pinned
to the existing append-only agent version, without duplicating that truth.

## Scope and acceptance

- Migration 00016 adds `session_input_snapshots` with forced tenant RLS and no
  application writes. An INSERT trigger captures the locked task for every new
  session, including old application binaries using unchanged INSERT syntax.
- Capture serializes against task edits and validates ready status and repository
  identity. Input/session identity and branch intent cannot be retargeted later.
- Explicit tenant-scoped sqlc/store reader joins only the session's pinned agent
  version. No public API, outbox payload, event or default log contains input text.
- Missing snapshots fail closed. Existing sessions remain without snapshots: it
  would be dishonest to reconstruct original task text from today's mutable row.
  Existing fake runs are unaffected; future real-runtime activation must refuse
  legacy sessions and require a new session.
- Test atomic rollback, raw old-style INSERT, edit/capture serialization, app-role
  write denial, RLS/wrong/no tenant, immutability, cascade and pinned settings.
- Migration is additive; old binaries need no changes. Down drops the new guards
  and table and loses captured text; never rollback while a consumer requires it.

## Non-scope and security

No runner wire format, prompt rendering, agent instruction field, secret retrieval,
provider registration, image or egress change. Task text remains untrusted data;
future delivery must use bounded structured stdin outside the checkout, never
argv/env/shell or repository policy files. Existing 50,000-character task bounds
are not proof of fitting the supervisor's 128-KiB encoded-input cap; future
assembly must explicitly reject oversize input without truncation or launching.
Agent capabilities and tool policy are pinned metadata, not approval grants.

Credential ownership/delivery requirements and unresolved account/funding choice
are recorded in proposed ADR-018. No real key is discovered, stored or delivered
by this slice. The fake adapter and M6 runtime activation gates remain unchanged.

## Verification

Run format, sqlc, unit/race tests, targeted database integration under owner and
app roles, full integration with dev workers quiescent, lint, typecheck and build.
Deliberate mutations must demonstrate snapshot immutability, tenant filtering and
capture serialization. Report synthetic evidence separately from live CLI proof.

## Verification record

- Six new database integration tests plus existing creation/atomic-rollback tests
  pass with the race detector against real PostgreSQL and the non-bypass app role.
  Includes a disposable migration database, real pre-00016 legacy data, Down/Up
  with captured rows, no backfill and recovery. Test databases/containers cleaned.
- Five deliberate mutations caught: allow snapshot edits; disable tenant policy;
  remove task lock; permit pin/intent edits; bypass explicit store workspace filter.
- Full unit/race tests, lint, golangci-lint, typecheck, Go/web build, govulncheck
  and npm high/critical audit pass. Existing moderate npm findings remain.
- Full integration attempted with isolated DB and test-scoped queues/streams.
  It caught a borrowed-repository error translation regression, now fixed while
  preserving the existing test. Seven workflow tests could not connect to local
  Temporal: inside-container health confirms nothing listening on port 7233.
  Re-run with Temporal workflows explicitly skipped passes DB/NATS/Docker tests;
  **not a full integration pass**. No user dev workers were stopped.
- Full format gate flags unrelated local `.claude/settings.local.json` and the
  pre-existing `.claude/worktrees/`; neither changed. Changed-file checks pass.
- Draft 00016 applied locally; revised function definitions synced atomically
  without Down/data loss. No existing snapshot row was rewritten/deleted.

## Next

Review this storage slice; decide provider account/credential ownership, then
build an authenticated runner input/credential handoff with immutable scope,
size bounds, secret-store integration and sandbox-safe delivery. Agent instruction
editing requires an explicit versioned field/contract in a later slice.
