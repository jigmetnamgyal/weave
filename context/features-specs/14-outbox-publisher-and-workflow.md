Read `CLAUDE.md` before starting

We're draining the outbox and starting a durable workflow (Unit M5.1),
which is the first time anything in this system acts on its own.

Every session created so far sits in `queued` with a row in
`outbox_events` that nothing reads. This unit is the reader.

## What this unit owes before it does anything new

**Two tests M4.2 could not write.** It designed the claim protocol — a
lease with an expiry, taken by one conditional `UPDATE ... RETURNING` —
documented it in `db/migrations/00007_create_sessions_and_outbox.sql`, and
left it **unexercised**, because nothing claimed a row. Those two
outcomes are still unproven:

- a lease that **expires after its publisher dies**, so the row is
  claimable again without anyone intervening;
- **two claimers racing** for one row, where exactly one wins.

Write both before writing a publisher worth trusting, and *watch them
fail* first. M4.2's own concurrency test passed against unfixed code
because pgxpool creates its second connection lazily and the handshake
outlasted the overlap; M5.0's race test only became evidence once a
check-then-insert implementation made it go red. A claim protocol nobody
has seen break is a design, not a mechanism.

`ListPendingOutboxEvents` reads what a claimer would find and claims
nothing. The claim itself is new here.

## The decision this unit turns on

**Where the publisher runs, and what that does to readiness.**

`services/api/main.go` already runs two sweeps as goroutines, so a third
is the path of least resistance — and it is worth refusing on purpose.
`context/architecture.md` lists **workflow workers** as their own
deployment unit, and `services/worker` exists as a `doc.go` naming this
milestone as its occupant. A publisher inside the API means every API
replica polls the same table, and it means the API process holds a
Temporal connection to serve requests that do not need one.

That second consequence is already written down and waiting. The tracker
says readiness probes PostgreSQL, Redis and NATS but **not** Temporal, and
records why: *"Add a Temporal probe when the session workflow in M5 makes
it a hard serving dependency."* This unit answers that. If the publisher
lives in the API, Temporal becomes a serving dependency and readiness must
say so. If it lives in the worker, the API stays unaffected and the
worker needs its own health story.

Decide it, record it in the tracker with the reasoning, and make the
readiness probe match the answer rather than leaving the note pending for
another milestone.

## Workflow identity is the second idempotency mechanism

Architecture, Session Execution, step 2: *the workflow id is the session
id.* That is not a naming convention, it is the property that makes a
duplicated publish harmless — Temporal refuses to start a second workflow
with an id already running, so an at-least-once publisher cannot produce
two workflows for one session.

It composes with what M5.0 built rather than duplicating it: the
idempotency key stops a retried *request* creating a second session, and
the workflow id stops a retried *publish* creating a second workflow for
the session that exists. Both are needed, and they protect different
seams.

Say which Temporal error means "already running" and treat it as success,
not failure. An at-least-once publisher that reports a duplicate as an
error will retry forever.

## What the workflow does, and what it may not do

The minimum that proves the loop, and no more: move the session from
`queued` to `provisioning`, then to `failed`, with a reason naming the
runner that does not exist yet.

**`provisioning` cannot go back to `queued`.** The transition table
forbids it — `CanTransition(provisioning, queued)` is `false`, and
`provisioning` may become `running`, `cancelling`, `failed` or `expired`.
An earlier draft of the M5 plan described exactly that loop and a reviewer
caught it. Adding the edge to make a first workflow tidy would be changing
a state machine to suit a demo; ending somewhere legal costs nothing.

This makes M5.1 the **first caller of `SessionStore.Transition`**, which
M4.2 built complete with optimistic concurrency and left unused. Expect
the first real use to find something the tests did not.

## Errors have two kinds and the distinction must be visible

`context/code-standards.md`: *"Retryable and terminal errors are distinct
and observable"*, and *"Temporal activities use bounded retries,
exponential backoff, timeouts, and non-retryable error classes."*

A session whose task was archived between creation and provisioning is
**terminal** — retrying cannot help, and a workflow retrying it forever is
worse than one that fails. A database that is briefly unreachable is
**retryable**. Classify them at the point the error is raised rather than
by string-matching in the workflow, and make a terminal failure land the
session in `failed` with a reason rather than leaving it in `provisioning`
for a person to find.

The outbox has the same distinction: a row that failed for a retryable
reason goes back with `attempts` incremented and `available_at` pushed
out; one that cannot ever succeed should stop being retried. Decide what
"stop" means — a dead-letter state, an attempt ceiling — and say so.
`context/code-standards.md` requires a quarantine path that retains
failure metadata without leaking customer content.

## Versioning, from the first workflow

*"Workflow code must support safe versioning while existing sessions are
in flight."* This is easiest to honour when there is exactly one workflow
and nothing in flight, and nearly impossible to retrofit later. Use
Temporal's versioning mechanism from the first edit, not the first
problem.

## Dependencies

The Temporal SDK is not in `go.mod` yet. Two hazards, both already paid
for once in this project:

- **`go get` has dropped the `toolchain` directive twice**, reintroducing
  standard-library CVEs that `govulncheck` then flags. Check `go.mod`
  after adding the SDK.
- **`make ci` must stay honest.** It gained `tidy-check` in M3.3 for
  exactly this reason; run it before assuming a dependency change is
  clean.

Temporal is already in the local stack (`infra/docker-compose.yml`,
`temporalio/auto-setup:1.25.2`) and `TEMPORAL_HOST_PORT` is validated
configuration. Nothing new is needed to run it.

## Tests

- A lease expires after its holder dies, and the row is claimable again.
  **Watch this fail** against a boolean-flag implementation.
- Two claimers race for one row; exactly one wins. **Watch this fail**
  against a read-then-write, with the pool warmed so they truly overlap.
- A claimed row that fails goes back with `attempts` incremented and
  `available_at` pushed out.
- A completed row is never claimed again.
- Publishing twice for one session produces **one** workflow, proven
  against a real Temporal rather than a mock — the duplicate-id behaviour
  is the mechanism, and a mock would assert our belief about it.
- The workflow moves a session `queued → provisioning → failed`, and the
  transitions are recorded with the version each observed.
- A transition against a stale version is refused — M4.2 has this at the
  store; this is its first use through a workflow.
- A terminal error does not retry; a retryable one does.

## Carried forward, and worth reading before starting

**From M5.0, on privileged helpers.** Its retention sweep was routed
through a `SECURITY DEFINER` function that was narrow in shape and wide
open in range: it took any interval, and `interval '0'` would have emptied
the table for every tenant. If this unit adds anything that runs past the
policies, bound its arguments, and assume a compromised caller rather than
a well-behaved one.

**From M5.0, on sweeps that report success.** Its prune deleted nothing
for a subtle reason — a background context has no tenant, and `FORCE` RLS
then matches no rows while reporting success. A publisher polling with a
background context will hit exactly the same wall. Check what your loop
actually reads before trusting that it read nothing because there was
nothing.

**From M4.2 and M5.0, on concurrency tests.** Both units shipped a
concurrency test that passed against broken code on the first attempt.
Warm the pool, instrument the interleaving, and see it go red.

### Check when done

- A session created through the API reaches `failed` on its own, with its
  transitions recorded and no human action.
- Its outbox row is completed, and a second publish of the same session
  produces no second workflow.
- A publisher killed mid-claim leaves a row that another publisher picks
  up once the lease expires.
- The readiness decision is recorded in the tracker, and the probe matches
  it.
- `go.mod` still carries its `toolchain` directive, and `govulncheck` is
  clean.
- `make ci` and `make test-integration` pass.
