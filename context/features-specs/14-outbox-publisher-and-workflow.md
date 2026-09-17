Read `CLAUDE.md` before starting

We're draining the outbox and starting a durable workflow (Unit M5.1),
which is the first time anything in this system acts on its own.

Every session created so far sits in `queued` with a row in
`outbox_events` that nothing reads. This unit is the reader.

## What this unit owes before it does anything new

**Two tests M4.2 could not write.** It designed the claim protocol — a
lease with an expiry, taken by one conditional `UPDATE ... RETURNING` —
documented it in `db/migrations/00007_create_sessions_and_outbox.sql`, and
left it **unexercised**, because nothing claimed a row:

- a lease that **expires after its publisher dies**, so the row is
  claimable again without anyone intervening;
- **two claimers racing** for one row, where exactly one wins.

Write both, and *watch them fail* first. M4.2's own concurrency test
passed against unfixed code because pgxpool creates its second connection
lazily and the handshake outlasted the overlap; M5.0's race test only
became evidence once a check-then-insert made it go red. Two for two.

**And a third the first draft of this spec missed.** `outbox_events` has
`leased_until` and **no fencing token** — which is precisely the bug
M5.0's review found in `idempotency_keys`, sitting in the sibling table.
The first draft cited that lesson under "carried forward" and did not
apply it. Publisher A pauses past its lease, B reclaims the row, A returns
and completes or releases **B's claim** — clearing B's lease, or marking
done work that B is still doing.

So the schema gains a claim token, reissued on every claim including a
reclaim, and **every** state update must match it — not completion and
retry, which is how the first correction of this section put it, but
anything that writes to a claimed row, the terminal outcome below
included. Enumerating two is how the third is missed, and the third here
is the one that quarantines somebody else's work.

Reject a stale operation rather than letting it write, and test that A
cannot mutate B's claim — including that A cannot terminate it.

## Decided here: the publisher runs in the worker

The first draft posed this as a decision for the implementer. It is
recorded here instead, because leaving it open leaves Temporal ownership
and the health contract unresolved, and both have consequences beyond this
unit.

`services/api/main.go` already runs two sweeps as goroutines, so a third
is the path of least resistance. It is refused on purpose:

- `context/architecture.md` gives **workflow workers** their own
  deployment unit, and `services/worker` exists as a `doc.go` naming this
  milestone.
- A publisher in the API means every API replica polls the same table.
- It would make Temporal a **serving dependency of the API**, so a
  Temporal outage would fail readiness for requests that never touch it.

**Readiness, settled.** The tracker has carried this since M1.1: the probe
covers PostgreSQL, Redis and NATS but not Temporal, with the note *"Add a
Temporal probe when the session workflow in M5 makes it a hard serving
dependency."* With the publisher in the worker, **it does not become
one**, and the API's probe is left alone. The worker needs its own health
story: it is not serving HTTP, so "ready" there means it can reach
PostgreSQL and Temporal, and the unit must say how that is observed.

Record the decision and this reasoning in the tracker.

## Workflow identity, and the case running-uniqueness does not cover

Architecture, step 2: *the workflow id is the session id.* That is the
property making a duplicated publish harmless — Temporal refuses to start
a second workflow with an id already **running**.

**Running is not the whole story, and the first draft stopped there.**
Reuse of a *closed* id is governed separately, by the workflow id reuse
policy. The sequence that matters: the publisher starts the workflow, the
workflow completes, and the publisher dies before recording completion on
the outbox row. The lease expires, another publisher claims the row, and
publishes again — now against a **closed** id. Under a permissive reuse
policy that starts a second workflow for a session that already ran.

So the unit must state, and test:

- the **reuse policy**, which should reject a duplicate id rather than
  allow one;
- how an "already started" response is recognised and treated as
  **success**, not failure. An at-least-once publisher that reports a
  duplicate as an error retries forever.

**And the guarantee has to be bounded by something we control.** Temporal
forgets closed executions eventually, so a reuse policy is a promise with
an expiry date — while the outbox row, if it can stay retryable
indefinitely, has no expiry at all. Those two together do not give "a
second publish never starts a second workflow"; they give it *until the
retention window passes*, after which the duplicate this section exists to
prevent becomes possible again, on the row least likely to be looked at.

Close it one of two ways, and say which:

- give the outbox row a **maximum age or attempt ceiling shorter than
  Temporal's retention**, so a row cannot outlive the memory that protects
  it — the simpler answer, and checkable;
- or add a durable **application-side** guard that does not depend on
  Temporal remembering anything.

Either way the boundary is a tested case, not an assumption.

This composes with M5.0 rather than duplicating it: the idempotency key
stops a retried *request* creating a second session; the workflow id stops
a retried *publish* creating a second workflow for the session that
exists. Different seams.

## The publisher has no tenant, and no identity

Two problems that look like one, and the implementation fails on either.

**No tenant context.** The publisher polls across workspaces from a
background goroutine. `SessionStore.ListPendingOutbox` and
`SessionStore.Transition` take a workspace argument but derive the tenant
from `context.Context`, and under `FORCE` row-level security an absent
context matches no row — a poll that reads nothing and reports success,
which is exactly how M5.0's retention sweep failed. Decide how the
publisher gets context: a narrowly-scoped privileged operation, or a
per-workspace pass. Say which, and test it as `weave_app` with no ambient
context, asserting a queued event **is** found rather than that the poll
merely succeeded.

**If it is privileged, bound it by ownership.** The guidance carried from
M5.0 said only "bound the arguments", which is not enough here. A
privileged claim must take the workspace and session identifiers **from
the row it claimed**, not from its caller, and validate their relationship
in the same transaction. Otherwise a caller-supplied pair is an
authorization bypass through a user-controlled key. Test it with
mismatched cross-workspace identifiers.

**No actor.** `authorizeActor` refuses a nil user — `if actor.UserID ==
uuid.Nil { return ErrPermissionDenied }` — so every mutation today
requires a member, and a workflow is not one. M4.2 already anticipated
half of this: `session_state_transitions.actor_user_id` is nullable
precisely because *"a timeout that expires a session is not attributable
to anyone"*. The write path has no equivalent. Decide what a system actor
is and how the authorization re-check treats it, and do not settle it by
having the workflow impersonate the member who created the session —
attributing an automated transition to a person is a false statement in an
append-only trail.

## Activities are redelivered, so they must be idempotent

An activity commits its transition and its acknowledgement is lost.
Temporal redelivers. The retry replays the same observed version, the
session has already moved, and the store correctly refuses it as a version
conflict — so the workflow sees a failure for work that succeeded.

The activity must recognise **its own already-applied transition** as
success, without writing a second one, while still refusing a genuinely
competing transition. Those are different situations and telling them
apart is the work. An implementation can satisfy every other test here and
still get this wrong on the first lost acknowledgement.

## Errors have two kinds, and terminal needs a representation

`context/code-standards.md`: *"Retryable and terminal errors are distinct
and observable"*, and activities use *"bounded retries, exponential
backoff, timeouts, and non-retryable error classes."*

A session whose task was archived between creation and provisioning is
**terminal** — retrying cannot help. A database briefly unreachable is
**retryable**. Classify at the point the error is raised rather than by
string-matching in the workflow, and land a terminal failure in `failed`
with a reason rather than leaving the session in `provisioning` for
someone to find.

**The outbox needs the same distinction, and it has nowhere to put it.**
The claim predicate is `completed_at IS NULL`, so a row that must stop
being retried has only two representations available: marked complete,
which lies, or left pending, which retries forever. Define an explicit
terminal or quarantine outcome that excludes the row from claims while
retaining failure metadata — `context/code-standards.md` requires a
quarantine path that keeps that metadata without leaking customer content
— and define how an operator replays one deliberately.

## What the workflow does, and what it may not do

The minimum that proves the loop: `queued → provisioning → failed`, with
a reason naming the runner that does not exist yet.

**`provisioning` cannot go back to `queued`.** The table forbids it —
`CanTransition(provisioning, queued)` is `false` — and an earlier draft of
the M5 plan described exactly that loop before a reviewer caught it.
Adding the edge to make a first workflow tidy would be changing a state
machine to suit a demo.

This makes M5.1 the **first caller of `SessionStore.Transition`**, which
M4.2 built with optimistic concurrency and left unused.

## Versioning, from the first workflow

*"Workflow code must support safe versioning while existing sessions are
in flight."* Easiest to honour when there is one workflow and nothing in
flight; nearly impossible to retrofit. Use Temporal's versioning mechanism
from the first edit, not the first problem.

## Dependencies

The Temporal SDK is not in `go.mod`. Two hazards already paid for once:

- **`go get` has dropped the `toolchain` directive twice**, reintroducing
  standard-library CVEs that `govulncheck` flags. Check `go.mod` after
  adding the SDK.
- **`make ci` must stay honest** — it gained `tidy-check` in M3.3 for this
  reason.

Temporal is already in the local stack
(`temporalio/auto-setup:1.25.2`) and `TEMPORAL_HOST_PORT` is validated
configuration.

## Tests

- A lease expires after its holder dies, and the row is claimable again.
  **Watch this fail** against a boolean-flag implementation.
- Two claimers race for one row; exactly one wins. **Watch this fail**
  against a read-then-write, with the pool warmed so they truly overlap.
- A publisher whose lease expired **cannot** complete, release or
  terminate the row that replaced it.
- A claimed row that fails goes back with `attempts` incremented and
  `available_at` pushed out; a terminal failure stops being claimed and
  keeps its metadata.
- Publishing twice for one session produces **one** workflow — tested both
  while the first is running **and after it has closed**, against a real
  Temporal rather than a mock, because the reuse policy is the mechanism
  and a mock would assert our belief about it.
- A row cannot outlive the protection: whichever bound was chosen above is
  exercised at its edge, so the guarantee is known to hold rather than
  assumed to.
- The workflow moves a session `queued → provisioning → failed`, with each
  transition recording the version it observed.
- A redelivered activity whose transition already committed reports
  success and writes no second transition; a genuinely competing
  transition is still refused.
- The publisher finds a queued event as `weave_app` with no ambient tenant
  context — asserting the event **is** found, not that the poll merely
  succeeded.
- A privileged claim refuses mismatched cross-workspace identifiers.

## Carried forward, and worth reading before starting

**From M5.0, on privileged helpers.** Its retention sweep was routed
through a `SECURITY DEFINER` function narrow in shape and wide open in
range: any interval, so `interval '0'` would have emptied the table for
every tenant. Bound what a privileged path accepts, and assume a
compromised caller.

**From M5.0, on sweeps that report success.** Its prune deleted nothing
because a background context has no tenant and `FORCE` RLS then matches no
rows — while reporting success. A polling loop hits the same wall.

**From M4.2 and M5.0, on concurrency tests.** Both shipped one that passed
against broken code on the first attempt.

**From this spec's own first draft.** It quoted the M5.0 fencing lesson in
this section and did not notice the outbox had the identical hole. Citing
a lesson is not applying it.

### Check when done

- A session created through the API reaches `failed` on its own, with its
  transitions recorded and no human action.
- Its outbox row is completed, and a second publish produces no second
  workflow — including after the first has closed, and including at the
  edge of whichever bound keeps that true.
- A publisher killed mid-claim leaves a row another publisher picks up
  once the lease expires, and cannot disturb it when it returns.
- The publisher placement, the readiness decision, the system-actor
  decision, the reuse policy and the terminal-outbox representation are
  each recorded in the tracker with reasoning.
- `go.mod` still carries its `toolchain` directive, and `govulncheck` is
  clean.
- `make ci` and `make test-integration` pass.
