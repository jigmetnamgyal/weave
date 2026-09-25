Read `CLAUDE.md` before starting

We're making the session workflow cut a real branch (Unit M5.2), which is
the first time anything in Weave writes to a customer's repository on its
own — no member watching, no request in flight, only the workflow.

M3.3 built `CreateBranch` in September and **no branch has ever been
created against a real repository**. It was built for a caller that did
not exist yet: the session workflow. M5.1 made that workflow real but left
it doing the minimum — `queued → provisioning → failed`, with a reason
naming the runner that does not exist. This unit puts the branch step
between provisioning and that ending, so a session now leaves a branch
behind even though it still cannot run. It **closes the oldest outstanding
gap in the project**.

## What this unit is, and what it is not

**Is:** one new activity, `CreateBranch`, executed after `MarkProvisioning`
and before the failure. On success the session **still fails** — there is
no runner until M5.4 — but the branch is real and stays. In M5.4 the same
activity precedes provisioning a runner rather than a failure; the branch
step does not move.

**Is not:** cloning, commits or pull requests. Cloning is the runner
(M5.4). Commits and pull requests are M8. This is a single `refs/heads/*`
appearing in someone's repository under the App's name.

The workflow shape becomes `queued → provisioning → (cut branch) → failed`.
The branch is cut **while the session is in `provisioning`** — cutting it
is part of getting ready to run, not a state of its own. No new session
state is introduced, and none should be: adding one to make a first
workflow tidy is the mistake M5.1's spec caught in an earlier M5 plan.

## The activity trusts the row, not the payload

The outbox payload carries two identifiers and nothing else, by M4.2's
design. The branch activity reads everything it needs from the **session
row** — repository id, branch name, base branch — exactly as the
transition activities do. A repository id copied into the payload could go
stale against the row it came from, and the payload travels through a queue
into replayed code where a repository reference has no business arriving as
untrusted input.

The branch name is **already decided and stored**. M4.2 derived it from the
session id at creation (`domain.SessionBranchName(sessionID, task.Title)`)
precisely so a retried activity finds the branch it made rather than
cutting a second. This unit reads that stored name; it does not generate
one. The base is the session's `BaseBranch`, empty meaning the repository
default, resolved by `CreateBranch` as it already does.

## The system creates this branch, and it is not a member

This is the crux, and it is the same wall M5.1 hit for transitions, now in
a path that writes to GitHub and to the audit trail.

`InstallationService.CreateBranch` takes a `domain.Membership`, gates on
`membership.Can(domain.PermissionRepositoryManage)`, and audits the
creation against `membership.UserID` — a non-nullable id. The workflow has
no member. Three things follow, and each must be decided here rather than
worked around:

- **Authorization already happened.** A member with `session:create`
  created the session, and creating a session *is* authorizing the branch
  it names. Re-checking `repository:manage` for a system actor that holds
  no role would either always fail or force the workflow to impersonate the
  session's creator — and impersonation is how an automated write becomes a
  false statement about a person. Introduce a system entry point —
  `CreateBranchAsSystem`, or the existing method taught to accept the
  system actor — that does **not** take a `Membership`, derives the
  workspace and repository **from the session row it was given**, and
  validates their relationship in the same read. This mirrors M5.1's rule
  for privileged claims: identifiers come from the row, never from the
  caller, so a mismatched pair cannot be an authorization bypass.

- **The audit cannot name a person.** `session_state_transitions.actor_user_id`
  is nullable for exactly this reason; the branch audit path is not.
  M5.1 built `SystemActor()` for transitions — reuse it. The
  `AuditBranchCreated` event this unit writes must attribute to the system,
  not to `session.CreatedBy`. Attributing an automated branch creation to
  the member who filed the task, hours earlier and without knowing a branch
  would be cut now, is a false statement in a trail that cannot be edited.

- **Tenant context.** The activity runs from the worker with no ambient
  tenant, and under `FORCE` row-level security an absent context matches no
  row — the failure mode M5.0 and M5.1 both hit. Set tenant context the
  same way the transition activities do (`postgresTenant(ctx, workspaceID)`),
  and test the activity as `weave_app` with no ambient context, asserting
  the repository **is** resolved rather than that the call merely returned.

## The audit that can be lost, and why the transition is the real record

M3.3 made a deliberate choice worth re-reading before touching this:
`resolveExisting` writes **no audit row** when it finds an existing branch,
because nothing distinguishes our own interrupted attempt from a branch a
human created that happens to point at the same commit — and a false audit
is worse than a missing one.

That choice has a consequence this unit inherits and must handle.
`CreateBranch` today returns the branch **and** an `ErrAuditNotRecorded`
error when the branch was cut but its audit write failed. Temporal
redelivers the activity. The retry re-runs `CreateBranch`, which now finds
the branch it made, matches it at the requested base, and returns it
through `resolveExisting` as idempotent success — **writing no audit**. The
branch-created audit is then lost permanently, on precisely the retry that
was supposed to recover it.

So do not lean on the separate `AuditBranchCreated` event as the durable
record of the fact. **Record the resulting SHA on the session itself, in
the same system transition that carries the workflow forward**, so the fact
is written idempotently by a path M5.1 already made safe against
redelivery. Concretely:

- add a nullable `branch_sha` (or equivalent) to `sessions`, populated when
  the branch is confirmed — the value M5.4's checkout needs anyway;
- write it as part of the transition the activity records, so a redelivered
  activity that finds the session already advanced treats it as success and
  writes nothing twice, exactly as `TransitionAsSystem` already does;
- keep the `AuditBranchCreated` event as a best-effort operator signal, not
  the source of truth — and if its write fails, the durable SHA on the
  session means the fact is not lost.

Decide and record whether the SHA lands on the session or in the transition
row; either is defensible, but say which and why.

## Errors have two kinds, and the failure has to be honest

`context/code-standards.md`: retryable and terminal errors are distinct and
observable. `CreateBranch` raises both, and the classification belongs at
the point the error is raised, not in string-matching in the workflow:

- **Terminal** — the grant was withdrawn (`RepositoryForUse` reconciles and
  refuses), the installation lacks `contents: write`, the base does not
  exist, the target is protected or governed by a ruleset, or the name is
  somehow invalid. Retrying cannot help. Land the session in `failed` with
  a reason naming the cause — "the installation no longer holds
  contents:write", not "branch creation failed" — carried through as a
  non-retryable Temporal error class, the same mechanism M5.1 used for
  `SessionNotFound` and `TransitionNotAllowed`.
- **Retryable** — GitHub briefly unreachable or a 5xx. The bounded retry
  policy already on the workflow's activities covers it.

A terminal branch failure must not leave the session in `provisioning` for
someone to find. It goes to `failed`, which the transition table permits
directly.

## Idempotency, end to end

Three layers already exist and this unit must not undo any of them:

- **The name is stable** (M4.2), so every attempt names the same branch.
- **`CreateBranch` is idempotent by name** (M3.3): a 422 for an existing ref
  at the requested base is translated to success, and at a different base to
  a refused conflict.
- **The transition is idempotent** (M5.1): a redelivered activity finding
  the session already moved reports success and writes no second row.

Together these mean: the publisher starts the workflow at most-once-that-
matters, the branch activity may run several times, and the repository ends
with **one** branch and the session with **one** recorded SHA. Test the
composition, not just the parts — run the branch activity twice against a
faked GitHub and assert one branch, one SHA, one transition.

## Versioning, honoured not retrofitted

The workflow already carries `workflow.GetVersion(ctx, "session-workflow",
DefaultVersion, 1)`. Inserting an activity changes the execution graph, so
a workflow that began under the old code must not suddenly attempt a branch
activity mid-replay. Bump the version guard and branch on it, so an
in-flight execution finishes on the path it started. There is nothing in
flight today, which is exactly why this is cheap to do now and painful to
retrofit later.

## The worker gains GitHub, and what that costs

`CreateBranch` reaches GitHub through the installation adapter, which mints
an installation token (cached in Redis, per ADR-005) and calls the App API
with the private key. Until now the worker (M5.1) talked only to PostgreSQL
and Temporal. This unit gives it:

- the **GitHub App private key** and app configuration in its environment —
  the same validated configuration the API already holds, now needed by
  `services/worker` too;
- **Redis**, for the installation-token cache.

Wire these into `services/worker/main.go` and its config, reusing the API's
construction rather than duplicating it.

**Readiness stays as M5.1 set it.** The worker is ready when it can reach
PostgreSQL and Temporal. GitHub and Redis are per-activity dependencies
with bounded retries, not serving dependencies — the API does not gate
readiness on GitHub either, for the same reason. Record that this was
considered and deliberately left out of the probe.

## Tests

- A session workflow moves `queued → provisioning`, cuts a branch against a
  faked GitHub, records the SHA, and reaches `failed` with a reason — the
  branch surviving the failure.
- The branch activity runs twice (redelivery) and leaves **one** branch,
  **one** SHA and **one** transition — the composition of all three
  idempotency layers, not each in isolation.
- A withdrawn grant, an installation without `contents: write`, a missing
  base, and a protected or ruleset-governed target each land the session in
  `failed` with a reason naming the cause, as a non-retryable class — no
  retry budget spent.
- A transient GitHub 5xx is retried and then succeeds, the session ending
  where a healthy run ends.
- The activity resolves the repository as `weave_app` with **no ambient
  tenant context**, asserting the repository is found — not that the call
  merely returned.
- The system entry point derives workspace and repository from the session
  row and refuses a mismatched cross-workspace pair.
- The branch-created audit, when it is written, attributes to the **system**
  and never to `session.CreatedBy`; and when the audit write fails, the SHA
  on the session still records that the branch exists.
- GitHub is faked at the HTTP boundary, as in M3.3 — the 422 handling, the
  token minting and the error classification are what is worth testing, and
  an interface mock skips them.

## Carried forward, and worth re-reading before starting

**From M3.3, on the audit that recovers nothing.** `resolveExisting`
deliberately writes no audit on the idempotent path, because a false audit
is worse than a missing one. That is why this unit does not make the
separate audit event the record of the fact — see above.

**From M5.1, on the system actor.** A workflow is not a member; a nil user
is refused by `authorizeActor`; and an automated write attributed to a
person is a false statement in an append-only trail. `SystemActor()` and
`TransitionAsSystem` exist for this — reuse them, do not impersonate.

**From M5.1, on privileged paths and tenant context.** Identifiers come
from the row, not the caller; bound what a privileged path accepts; and a
sweep or activity with no tenant context matches no row under `FORCE` RLS
while reporting success. Test with no ambient context and assert a positive
result.

**From M3.1 and M3.3, on external writes and the local record.** The
delivery path took three attempts because each fix traded one failure mode
for another; M3.3 shipped three regressions in one seam — what happens when
the external write succeeds and the local record does not. That seam is
exactly the branch-cut-then-SHA-not-recorded case above. Write down what
must happen for each outcome before choosing the mechanism.

### Check when done

- A session created through the API reaches `failed` on its own, **with a
  real branch cut in the repository** and its SHA recorded on the session —
  no human action.
- Running the branch activity again produces no second branch, no second
  SHA, no second transition — including after the first has closed.
- A withdrawn grant, a missing `contents: write`, a missing base and a
  protected target each fail the session with a reason that names the cause,
  without spending the retry budget.
- The system-actor decision, the SHA-recording location, the worker's new
  GitHub and Redis dependencies, and the readiness decision are each
  recorded in the tracker with reasoning.
- No token or key appears in a log line, verified by grep.
- `go.mod` still carries its `toolchain` directive, and `govulncheck` is
  clean.
- `make ci` and `make test-integration` pass.
