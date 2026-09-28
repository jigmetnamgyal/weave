Read `CLAUDE.md` before starting

We're building the event contracts and the ingestor (Unit M5.3), which is
the first time Weave accepts input from the execution plane — the side of
the system that runs model output and customer code.

Everything the control plane has consumed so far came from a person through
an authenticated request, from GitHub through a signed webhook, or from our
own outbox. A runner is none of those. It runs a coding agent inside a
customer's repository, and by the architecture's own threat model **what it
sends is untrusted**. Its events carry provider output and fragments of
customer source into the one table every session's history will be read
from. This unit is shaped around that.

`contracts/events` has been a placeholder since M1.1, naming M5 as its
occupant. This fills it.

## What this unit is, and what it is not

**Is:** the versioned event envelope and the first event types, as schemas
with compatibility fixtures; a JetStream stream; an ingestor that validates,
deduplicates, sequences and persists into an append-only `session_events`
table; a quarantine for what it refuses; and a small publisher that M5.5's
fake provider will use to send events.

**Is not:**

- **The runner.** M5.4. Nothing in this unit produces events in production;
  the producer here is a library, exercised by tests.
- **Runner identity.** The dedup key includes `runner_id`, and the column
  exists, but binding a runner to a session — so a runner cannot emit for a
  session it was not provisioned for — needs a runner record that does not
  exist until M5.4. See "The gate this unit hands to M5.4" below.
- **Reading events.** No API endpoint and no realtime gateway; those are M6.
  A read endpoint added now would be the twelfth unpaginated list, over the
  table that grows fastest, and the tracker gates pagination before M6.
- **Artifacts.** Large payloads are refused here, not offloaded. Object
  storage does not exist yet.
- **Tool, approval and diff events.** M7. This unit defines the envelope
  and only the types M5.5 needs to demonstrate the chain.

## Decided here: the ingestor is its own deployment unit

`context/architecture.md` lists the event ingestor as a horizontal scaling
role beside the API and the workflow worker. It gets `services/ingestor`,
not a goroutine in the worker, for the reasons M5.1 kept the publisher out
of the API:

- Ingestion scales with runner output; workflows scale with sessions. Tying
  them together means scaling one to relieve the other.
- A worker whose Temporal connection is down should not stop ingesting, and
  an ingestor stuck on a poison message should not stall workflows.
- Readiness stays honest: the ingestor is ready when it can reach NATS and
  PostgreSQL, and has nothing to say about Temporal.

`make dev` starts it. Record the decision and reasoning in the tracker.

## The contract

Names and envelope follow `context/code-standards.md` and
`contracts/events/README.md`, which already say what every event carries.
This unit makes that true in files rather than in prose:

- **An envelope schema**: `event_id` (UUIDv7, producer-generated),
  `schema_version`, `type` (the normalized type), `occurred_at`,
  `producer` (a runner id), `workspace_id`, `session_id` (the aggregate),
  `correlation_id`, and `payload`. W3C trace context travels in NATS
  headers, not the envelope.
- **Transport names** use `<domain>.<entity>.<action>.v<version>`; the
  normalized type stored in history drops the transport version. Say where
  the mapping lives and test it.
- **The first types**: `message.created` and `provider.failed` — the two
  M5.5's fake provider needs to show a session producing output and ending
  in a provider failure. `plan.updated`, `tool.*`, `file.changed` and the
  rest arrive with the units that emit them.
- **Compatibility fixtures** for each type: a valid example, one with an
  unknown extra field (must be accepted, field ignored), and one with an
  unknown major version (must be quarantined). A test decodes every fixture
  through the ingestor's own decoder, so the schema and the Go code cannot
  drift apart unnoticed — the same shape as M3.2's contract drift check.

**Unknown types are quarantined, not stored.** The standards say unknown
fields are ignored and unknown major versions rejected; they are silent on
unknown *types*. Storing one means persisting something nothing downstream
can render or validate, from an untrusted producer. Record the decision.

Whether schemas are enforced by a JSON Schema validator or by strict Go
decoding checked against the fixtures is the implementer's call — but if a
validator library is added, see Dependencies.

## Transport

- **One stream**, file-backed, subjects `weave.session.<session_id>.events`.
  Session ids are opaque UUIDs, which satisfies the architecture's rule that
  subjects carry opaque tenant identifiers and never names.
- **Retention is operational**, per the architecture: bounded by age and
  size, because `session_events` is the permanent history and the stream is
  not. State the limits and why.
- **Who creates the stream** is decided and idempotent — the ingestor at
  startup, creating or verifying it. A stream whose config has drifted from
  what the code expects is reported loudly, not silently reconfigured.
- **A durable pull consumer** with explicit acknowledgement, a stated ack
  wait and a stated maximum delivery count.
- **The publisher** sets `Nats-Msg-Id` to the event id, so JetStream's
  duplicate window absorbs most producer retries. That is an optimisation,
  not the guarantee: the window expires, and the database constraint below
  is what makes a duplicate harmless.

## Every field is untrusted, including the ones that look like ours

The envelope carries `workspace_id` and `session_id` because the standards
require it. **Neither may be believed.** M5.1 and M5.2 both learned that
identifiers come from a row, never from a caller, and a runner is the least
trustworthy caller the system has.

- The **session** comes from the subject, and must equal the envelope's
  `session_id`. A mismatch is quarantined — it means a producer publishing
  on one session's subject about another.
- The **workspace** comes from the session row, and must equal the
  envelope's `workspace_id`. A mismatch is quarantined and logged as a
  security event, because a runner naming another tenant is the attack this
  field makes possible if it is trusted.
- **`occurred_at` is the producer's claim.** Store it, and store
  `received_at` beside it; never order by `occurred_at`.
- **Payloads are provider output and may contain customer source.** Never
  logged, never in a metric label, never in a quarantine row. Bound the
  envelope size and refuse oversized events to quarantine — "payloads stay
  small" is a rule, and this is where it is enforced.

### The ingestor has no tenant

It consumes every session's subject, so it starts with no workspace — the
wall M5.0's sweep and M5.1's publisher both hit, where `FORCE` RLS matches no
row and the read reports success. Resolve the session's workspace through a
narrowly-scoped privileged lookup — one session id in, one workspace out —
and do everything after it in an ordinary tenant transaction scoped by that
workspace. Bound the privileged function and assume a compromised caller:
it must not be usable to enumerate sessions. Test as `weave_app` with no
ambient context, asserting the event **is** persisted.

## Deduplication and ordering

**Deduplicate by `(session_id, runner_id, event_id)`**, as the architecture
states, enforced by a unique constraint rather than by a read before the
write. M5.0 and M4.2 both shipped a check-then-insert that passed its
concurrency test until the test was made to overlap for real.

**Sequence per session, gapless, assigned at ingestion.** The sequence is
the order of acceptance, which is the only order the control plane can
vouch for; invariant 9 says history is ordered, and `occurred_at` is a
claim. Two requirements pull against each other and both must hold:

- two ingestor replicas accepting different events for one session must not
  assign the same sequence — serialise per session (a counter under a row
  lock is the obvious shape);
- a **duplicate consumes no sequence**. A redelivered event that is
  refused as a duplicate after taking a number leaves a gap, and a reader
  that treats gaps as missing events will wait for one that never comes.

Test both with the pool warmed and the writers genuinely overlapping, and
**watch each fail first** — against a read-then-insert for the duplicate,
and against a `max(sequence) + 1` without a lock for the numbering.

## History is append-only

`session_events` gets the treatment `session_state_transitions` has: a
trigger refusing UPDATE and DELETE, a statement trigger refusing TRUNCATE,
`SELECT, INSERT` grants and nothing more, and row-level security **in the
creating migration**. Corrections are new events, never edits — invariant 9.

**Events for a terminal session** are refused to quarantine. Invariant 10
makes terminal history immutable, and an event arriving after `completed`
or `failed` would be a rewrite of it. This may be too strict once a real
runner's final events race the terminal transition; if so, M5.4 revisits it
with a runner that can show the race, rather than this unit guessing.

## Ack, retry and quarantine

Write down the outcome for each case before choosing the mechanism — the
M3.1 lesson, where each fix to the delivery path traded one failure mode for
another:

| Case | Outcome |
| --- | --- |
| Persisted | ack **after** the transaction commits |
| Duplicate | ack; nothing written, no sequence consumed |
| Transient (database unreachable) | no ack; redelivered after backoff |
| Invalid: schema, unknown type, unknown major version, oversize, subject/envelope mismatch, workspace mismatch, unknown session, terminal session | quarantine row, then ack (terminate) |
| Delivered as many times as the consumer allows | quarantine **before** JetStream gives up, never dropped silently |

The last row is the one that is easy to miss: at the delivery ceiling
JetStream stops redelivering, and without a check of the delivery count the
event simply vanishes. The ingestor must quarantine on the final attempt.

**The quarantine** keeps what an operator needs and nothing a tenant would
not want kept: received time, subject, reason code, the event id and session
id if they parsed, schema version, size, delivery count, and a hash of the
payload for correlation — **no payload**, per the standards. An event that
failed before its session was resolved has no workspace, so decide whether
this table is tenant-owned and how an operator reads it; say which and
test that `weave_app` cannot read another tenant's quarantine rows.

## The gate this unit hands to M5.4

Until a runner is bound to its session, anything that can publish to the
stream can write history for any session whose id it knows. This unit
narrows that — subject and envelope must agree, the workspace must match the
row, terminal sessions are closed — but cannot close it. Record in the
tracker, as a gate on M5.4, that the ingestor must refuse events from a
`runner_id` not provisioned for the session, and that NATS publish
permissions must restrict each runner to its own session's subject.

## Dependencies

`github.com/nats-io/nats.go` is already required; its `jetstream` package
needs no new module. A JSON Schema validator, if chosen, would be new — and
**`go get` has dropped the `toolchain` directive twice**, reintroducing CVEs
`govulncheck` flags. Check `go.mod` after adding anything. `make ci` must
stay a superset of CI.

## Tests

- Every fixture decodes as its name says: valid is persisted, unknown field
  is persisted with the field ignored, unknown major version is quarantined.
- An event is persisted as `weave_app` with no ambient tenant — asserting
  the row **exists**, not that the handler returned.
- A duplicate delivery writes nothing and consumes no sequence; two
  ingestors racing the same event persist it once. **Watch it fail** against
  a read-then-insert.
- Two ingestors accepting different events for one session produce a
  gapless sequence with no repeats. **Watch it fail** against an unlocked
  `max + 1`.
- A subject/envelope session mismatch, a workspace that does not match the
  session row, an unknown session, a terminal session, an unknown type and
  an oversized event are each quarantined with the right reason and **no
  payload**.
- A transient database failure is not acked, and the event is persisted on
  redelivery.
- An event on its final permitted delivery that still cannot be persisted is
  quarantined rather than dropped.
- UPDATE, DELETE and TRUNCATE on `session_events` are refused.
- `weave_app` cannot read another workspace's events or quarantine rows.
- The privileged session lookup cannot be used to enumerate sessions.
- Against a **real** JetStream, not a mock — redelivery, ack wait and the
  delivery ceiling are the mechanism, and a mock would assert our belief
  about them. M5.1 made the same call for Temporal's reuse policy.

## Carried forward, and worth re-reading before starting

**From M5.1 and M5.2, on identifiers.** Identifiers come from the row, not
the caller. Here the caller is a runner, and the envelope is the caller.

**From M5.0 and M5.1, on privileged paths.** Bound what a privileged
function accepts; a tenantless read matches no row under `FORCE` RLS while
reporting success.

**From M4.2, M5.0 and M5.1, on concurrency tests.** Three units shipped or
nearly shipped one that passed against broken code. Watch it fail.

**From M3.1, on at-least-once delivery.** Every earlier fix to the webhook
path traded one failure for another. The table above is the answer to that
lesson; do not start from the mechanism.

**From M5.2's review, on classification.** Its first fix named every 403 a
refusal and made rate limits terminal. Classify each quarantine reason by
whether waiting could change it, and do not let a transient failure be
quarantined as invalid.

### Check when done

- An event published through the publisher appears in `session_events`
  with sequence 1, and a second with sequence 2; publishing the first again
  changes nothing.
- Each refusal case lands in quarantine with its reason and no payload.
- A killed ingestor loses nothing: unacked events are redelivered and
  persisted once.
- The deployment-unit decision, the unknown-type decision, the stream
  limits, the quarantine's tenancy, and the M5.4 runner-binding gate are
  each recorded in the tracker with reasoning.
- `go.mod` still carries its `toolchain` directive, and `govulncheck` is
  clean.
- `make ci` and `make test-integration` pass.
