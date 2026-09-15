Read `CLAUDE.md` before starting

We're adding the record a session *is* (Unit M4.2): the row that says
someone asked a specific agent to do a specific task, the state it sits
in, and the durable promise that something will pick it up.

Still nothing runs. What changes is that for the first time a write in
this system has a *consequence somewhere else* — and the whole unit is
about making that promise survive a crash between the two.

## Two corrections to what the tracker says

Both are mine, and both narrow the unit.

**It is not "the eighteen-state machine".** `context/architecture.md`
lists **sixteen** states. I have written eighteen twice. Count them
before implementing, and treat the architecture list as the authority:
`draft`, `queued`, `provisioning`, `running`, `waiting_for_input`,
`waiting_for_approval`, `pausing`, `paused`, `resuming`, `review_ready`,
`finalizing`, `completed`, `cancelling`, `cancelled`, `failed`,
`expired`.

**It is not the first caller of `CreateBranch`.** I said this in the
tracker and again when closing M4.1, and it is wrong. Architecture,
Session Execution, step 3: *the workflow* validates quota and GitHub
access and creates the branch. The workflow is Temporal, Temporal is
M5, and the SDK is not even a dependency yet — `services/worker` is a
`doc.go` saying "no logic until M5".

Creating the branch inline would also break two things on purpose:
invariant 2 says request handlers never do long-lived work inline, and a
GitHub round trip inside `CreateSession` would put a network call in the
middle of the transaction that is supposed to be atomic. A branch
created and then rolled back is a branch nobody knows about.

So M4.2 writes the branch **intent** and stops. M5 acts on it.

## Why this is its own unit

M4.1 delivered the definitions. This is the lifecycle, and it is where
the outbox arrives — a mechanism the rest of the system depends on and
nothing has needed until now. That is enough for one unit without the
state machine's *transitions* also being wired to endpoints.

Explicitly out of scope, each deferred to where the consumer lives:
the Temporal workflow and the outbox publisher (M5), quota (M5, step 3),
session events and the realtime gateway (M6), tool policy enforcement
(M7).

## The decision this unit turns on

**Every asynchronous consequence is a row written in the same
transaction as the thing that caused it.**

The alternative — write the session, then publish — has a window where
the session exists and nothing will ever pick it up, and the window is
open exactly when the process dies. A user sees a queued session that
stays queued forever, and no amount of retrying the request fixes it,
because the session is already there.

So `CreateSession` writes the session, its participants, the initial
state transition, the policy snapshot, the branch intent, and the
outbox record in **one** transaction. Any of those failing fails all of
them, and the caller can simply retry.

## Write down the outcomes before choosing the mechanism

This is the M3.1 lesson, and the outbox is the same shape as the
delivery path that took three attempts — something is claimed, worked,
and acknowledged, and each fix there traded one failure mode for
another. Before writing the table, write down what must happen when:

- nothing has claimed the row yet;
- a publisher claimed it and succeeded;
- a publisher claimed it and failed;
- a publisher claimed it and **died holding the claim**;
- the same row is claimed twice concurrently.

The last two are what the M3.1 path got wrong twice. Decide them on
paper, then make the columns follow — attempt count, next-attempt time,
and whatever represents a claim need to exist because an outcome
requires them, not because an outbox usually has them.

**The publisher is not built here.** Rows will accumulate with nothing
draining them, which is what a queue does before its consumer exists —
unlike the webhook subscriptions M3.1 deferred, where the traffic would
have been *lost* rather than waiting. But the table must be shaped for
the consumer now: retrofitting a claim protocol onto rows already being
written is precisely how M3.1 took three goes.

## Data

All workspace-owned, all with `ENABLE`/`FORCE ROW LEVEL SECURITY` and
policies **in the creating migration**. This is the third time of
saying it and it has been right every time: RLS is off by default, so a
tenant table added without it is silently unprotected and looks
entirely correct.

- `sessions` — `workspace_id`, the task, the pinned `agent_version_id`,
  the state, an integer `version` for optimistic concurrency, who
  created it, timestamps. A continuation links to the session it
  continues (invariant 10), so the column exists now even though nothing
  sets it.
- `session_participants` — who is in a session and in what capacity. The
  creator on creation.
- `session_state_transitions` — previous state, next state, reason,
  actor, timestamp, and the session version the transition observed.
  **Append-only, enforced by trigger**, both triggers, the way
  `agent_versions` now is. Note the cascade problem is already solved
  there: read that trigger before writing this one rather than
  rediscovering it.
- `outbox_events` — the durable promise.

## The pinned version, and what a policy snapshot adds

A session references `agent_version_id`, not `agent_id`. That is what
M4.1 built the version table for.

Which raises a question this unit must answer rather than assume: **an
agent version is already immutable, so what does a separate policy
snapshot hold that the pinned version does not?**

Either it holds nothing new and the pin *is* the snapshot — defensible,
and it means one fewer table — or a session's policy can be narrower
than its agent's, in which case the snapshot is the narrowed result and
the session is readable against what it actually ran under. Pick one,
say which in the tracker, and do not create a table that duplicates
`agent_versions` because the architecture document lists a name.

## The branch intent and its name

The intent is the branch the workflow will create: the repository, the
base, and the **name**, decided here and stored.

Deciding it here is the point. M3.3's endpoint requires a caller-supplied
name because it had no stable identity to derive one from, and an
earlier revision that generated a name per attempt made retries create a
*second* branch. The session id is that identity, and the name must be a
pure function of it — so the workflow retrying an activity finds the
branch it already made.

`domain.NewBranchName` is **not** reusable as it stands: its suffix
comes from `crypto/rand`, which is exactly the non-determinism M3.3
rejected. Derive the suffix from the session id instead, keep the
existing validation, and keep the random generator only if something
still needs it.

## Authorization

`session:create`, which already exists and already means this. It is
also the permission M4.1 reused for tasks, and the reasoning there was
that a task is the input to a session — this is the unit that makes that
claim testable rather than asserted.

Membership decides visibility, permission decides the operation, as
throughout. A session references a task and an agent version, and both
must belong to the caller's workspace: check it, and prefer a composite
foreign key over a handler check, as `tasks` does for repositories.

A session cannot be created from a task that is not `ready` — and
`ready` already guarantees a repository, which is what the branch intent
needs.

## The state machine

Sixteen states, in the domain, as a transition table.

Only one edge is exercised by an endpoint here — creation. Build the
whole table anyway, and make a test fail when a state pair is added
without deciding it, the way `TestMatrixIsExhaustive` does for the
authorization matrix. The cost of the table is small and it is the only
artifact that says what the sixteen states *mean*; the cost of
discovering in M6 that two states have no defined relationship is not.

Transitions carry the session version they observed and fail when it has
moved. Terminal states — `completed`, `cancelled`, `failed`, `expired` —
have no outgoing edges at all, which is invariant 10 stated as data.

## Tests

- Creating a session writes session, participant, transition, snapshot,
  intent and outbox row, and **none of them exists** when the
  transaction fails. Prove the failure case, not just the success —
  inject a failure on the last write and assert the first is absent.
- The branch name is a pure function of the session id: same id, same
  name, twice.
- A transition against a stale version is refused.
- A terminal session refuses every transition.
- A task that is not `ready` is refused; a task from another workspace is
  a 404, not a 403.
- `session_state_transitions` refuses UPDATE, DELETE and TRUNCATE
  against real PostgreSQL, and deleting a workspace still works.
- A viewer cannot create a session; the API refuses them.
- Cross-tenant: the new tables return nothing to `weave_app` with no
  tenant context, and nothing when another tenant's context names this
  workspace. Connect as `weave_app` or the test proves nothing.

## Carried forward, and worth reading before starting

**From M4.1, on comments that outlive the code they describe.** The
first version of the task patch fix carried comments crediting a row
lock for safety that `LockWorkspace` was actually providing. The comment
was plausible, wrong, and would have taught the next reader the wrong
thing about why the code was safe. When writing why something is
correct, check that it is the reason.

**From M4.1, on concurrency tests.** The first concurrent-patch test
passed against the unfixed code, because the two requests never
overlapped — pgxpool creates its second connection lazily and the
handshake outlasted the window. A concurrency test nobody has watched
fail is not evidence. Break the thing on purpose and see the test go
red before trusting it.

**From M3.3, on where a test lives.** If the behaviour is "what the API
returns", the test belongs at the transport. A service test asserting
the right value passed while the handler discarded it.

### Check when done

- A session is created from a ready task and appears with its initial
  state, its participant, and one transition.
- Its branch intent names a branch that does not exist yet, and the name
  is stable across reads.
- An outbox row is present and unclaimed.
- A failed transaction leaves nothing behind, proven.
- The append-only triggers refuse UPDATE, DELETE and TRUNCATE.
- Cross-tenant reads return nothing, verified as `weave_app`.
- The policy-snapshot decision is recorded in the tracker with its
  reasoning, and the outbox outcomes are written down before the table.
- `make ci` and `make test-integration` pass.
