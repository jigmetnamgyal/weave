Read `CLAUDE.md` before starting

We're making retryable mutations safe to retry (Unit M5.0), which is the
gate M4.2's review put in front of everything else in M5.

## Why this is first, and why it is small

`context/code-standards.md` line 100: "Mutations that may be retried
require an idempotency key and a documented idempotency scope." Nothing
in this project has one. `idempotency_keys` is in the architecture's
table list and unbuilt.

Today that is survivable. A lost response on `POST /sessions` leaves a
duplicate row and a duplicate outbox record — visible, recoverable,
annoying. **The moment M5.1 ships the outbox publisher, the same retry
starts two workflows and cuts two branches in a customer's repository**,
and invariant 5 — every external side effect is idempotent — stops being
satisfied.

So this lands before the publisher, not after. It is the cheapest unit in
M5 and the only one that gets more expensive by waiting.

## Scope: where a key is required, and where it is not

The architecture is deliberately narrow about this, and the narrowness
should be kept:

> API mutations accept idempotency keys **where retries may duplicate
> side effects.**

Ten `POST` operations exist. Most do not need a key, and giving them one
would be ceremony that future readers have to justify:

- **`createSession` needs it.** A duplicate becomes two workflows and two
  branches once M5.1 exists. This is the unit's real subject.
- `acceptInvitation` is already idempotent — the token is single use, and
  a second attempt is refused by the conditional UPDATE that makes it so.
- `createBranch` is already idempotent by name, which M3.3 established
  after getting it wrong once.
- `receiveGitHubWebhook` is already deduplicated by delivery id.
- `issueWorkspaceInvitation` is protected by the one-outstanding-per-
  address rule.
- `createWorkspace`, `createTask`, `createAgent`, `addAgentVersion` and
  `beginGitHubInstall` produce duplicates that are visible and
  deletable, not external.

**Build the mechanism generally; apply it to `createSession` only.**
Adopting it elsewhere should be a decision someone makes with a reason,
recorded at the time.

## The decision this unit turns on

**A key scopes a result, not a request.**

The naive version stores "this key was seen" and returns early on the
second attempt. That is wrong in the case that matters: the first attempt
may still be in flight, and answering "already done" to a caller whose
work has not finished is how the M3.1 delivery path acknowledged
something GitHub then never retried.

So the record has three states, and they must be written down before the
columns are:

- **unseen** — this is the first attempt; claim the key and proceed.
- **in flight** — an attempt holds the key and has not finished. The
  right answer is a refusal the caller can retry, not a fabricated
  success.
- **complete** — the stored response is replayed. "Verbatim" is not
  precise enough to implement, so: the original **status code**, the
  original **body bytes** (not a re-encoding, which would let a field
  order or a formatting change slip in between the two answers), and the
  headers that describe the body — `Content-Type` and `Cache-Control`.

  **`X-Request-Id` is the one that needs deciding rather than
  defaulting.** `httpx.WithRequestID` sets it on every request before the
  handler runs, so a replay either echoes the *retry's* id or resurrects
  the *original's*, and those are different products: the first lets an
  operator find this call in the logs, the second points at the call that
  did the work. Pick one, say why, and test it — a retry sent with a
  different request id should assert status, exact body bytes, content
  type, cache policy, and whichever request-id behaviour was chosen.

### The claim cannot be in the same transaction as the work

An earlier draft of this spec said it must be, "for the same reason the
outbox row is". That is wrong, and the contradiction is worth stating
because it is the whole difficulty of the unit.

A claim written inside the work's transaction is invisible until that
transaction commits. A second request then does not see it — it **blocks**
on the unique index until the first commits or rolls back, and wakes to
find `complete`. It never observes `in flight`, so the second of the
three states above is unreachable and a caller waits out the full
duration of someone else's request instead of being told to retry.

Making the second state reachable means the claim commits **before** the
work — which reintroduces exactly what the single transaction was
avoiding: a claim held by a process that then dies. That is the M3.1
shape again, and the answer it eventually reached was a lease with an
expiry, which `outbox_events` now uses.

So the unit must choose between:

- **claim in its own transaction with a lease** — the second state works,
  and a dead holder is recovered when the lease expires; or
- **claim inside the work's transaction** — simpler and self-releasing on
  rollback, at the cost of concurrent callers blocking rather than being
  refused.

Decide, record which and why in the tracker, and say what a refused
caller is told: the status code and whether anything indicates when to
retry. Do not restate the contradiction as resolved without saying how.

## Scope means scope

A key is not global. The same string from two workspaces, or two users,
or against two different endpoints, must not collide — and a key replayed
against a *different request body* is a caller bug that must be refused
rather than silently answered with the first result.

**The scope is `(workspace_id, user_id, endpoint, key)`.** Stated here
rather than left to the implementation, because the standard requires a
documented scope and a scope nobody wrote down is one every future
endpoint invents again. Workspace and user are in it because a key is a
caller's string and two callers may pick the same one; the endpoint is in
it because the same string against `createSession` and against some later
mutation are unrelated requests.

**Body comparison is over a canonical fingerprint, and the canonical
rules are the part that goes wrong.** A raw byte comparison rejects
retries that are equivalent — a client that reorders JSON members, or
adds whitespace, or sends `base_branch` as `""` where it omitted it
before, has not changed its request. `createSession` takes `task_id`,
`agent_version_id` and an optional `base_branch` where omitted means the
repository default, so "omitted" and "empty" are the same request and
must fingerprint the same. Define the normalisation explicitly, and test
the equivalences rather than only the differences.

## Transport

`Idempotency-Key`, a request header, optional **at the API** and
**required of our own client**. Those are different things and the unit
needs both.

Optional at the API because `context/architecture.md` says so — "an
authenticated request with a request ID and optional idempotency key" —
and because a request without one must keep behaving exactly as it does
today. This unit must not fail an existing caller for want of a header it
has never sent.

But a mechanism no caller uses protects nothing, and today no caller
would: `apps/web/lib/api.ts` sends only a JSON body, and the contract
describes no such header. **A duplicate branch does not care that the
protection was available.** So the unit also:

- adds the header to the contract, so the generated types carry it;
- accepts an optional key in the `createSession` wrapper and forwards it
  when present, leaving calls without one unchanged;
- makes the session form send a **stable** key — stable across a retry of
  the same submission and different across a deliberate second one. A key
  regenerated per attempt is worse than none, because it looks like
  protection: that is the mistake M3.3 made with branch names, where
  every attempt produced a different name and a retry created a second
  branch.

## Data

`idempotency_keys`, workspace-owned, with `ENABLE`/`FORCE ROW LEVEL
SECURITY` and policies **in the creating migration**. Fourth time of
saying it; still true.

Records need at least: the key, its scope, what became of it, the stored
response, and enough to expire rows that will never be replayed. Let the
three outcomes above decide the columns rather than copying a shape from
elsewhere.

**A unique constraint on the canonical scoped key is required, not
optional.** Without it the implementation is a check followed by an
insert, and two concurrent requests both check, both find nothing, and
both create a session — which is the entire failure this unit exists to
prevent, reintroduced by the storage layer. The constraint is what makes
exactly one claim win; it does **not** by itself produce the in-flight
refusal, which is the separate decision above. Define what the losing
insert does and cover it with the concurrent test.

**Retention has to be decided here, and it has not been elsewhere.** The
seven days in `services/api/main.go` is the webhook delivery log's and
its reasoning is about GitHub's redelivery window, not about how long a
caller might retry. The audit-retention policy in
`context/architecture.md` does not name this table. So either set a
period with a reason, or name the audit-retention policy as a dependency
and state the interim contract — and in both cases say what a retry
after expiry does. It creates a new session: that is defensible, it is
what deleting the record means, and it must be written down rather than
discovered by someone whose retry produced a second branch a fortnight
later.

**Stored responses are customer data.** A replayed `createSession`
response carries a branch name and identifiers, which is a second reason
the retention answer cannot be "keep them forever".

## Tests

- The same key, twice, returns the same session and creates **one** row.
  Prove the second attempt wrote nothing, not merely that it answered the
  same.
- Two concurrent requests with one key: one succeeds, the other is
  refused as in flight, and exactly one session exists. This is the test
  that fails against a check-then-act implementation, and it needs real
  PostgreSQL to mean anything — watch it fail before trusting it.
- The same key with a different body is refused.
- The same key from another workspace is a different key.
- A request with no header behaves exactly as it does now.
- A key held by a transaction that rolled back does not block the retry.
- A retry sent with a *different* request id returns the stored status,
  the same body bytes, the same content type and cache policy, and
  whichever `X-Request-Id` behaviour was chosen.
- Equivalent bodies fingerprint the same: reordered JSON members, added
  whitespace, and `base_branch` omitted versus sent empty.
- The web form sends the same key when a submission is retried and a
  different one for a fresh submission.

## Carried forward, and worth reading before starting

**From M3.1, on anything that claims, retries or leases.** That path took
three attempts and each fix traded one failure mode for another:
deduplicating before the effect dropped failed retries; recording
completion separately allowed concurrent double-execution; a lease
prevented that but acknowledged work still in flight. This unit is the
same shape. Write down what must happen for each outcome before choosing
the mechanism — M4.2 did that for the outbox and it held.

**From M4.2, on tests that cannot fail.** Its first concurrency test
passed against the unfixed code, because pgxpool creates its second
connection lazily and the handshake outlasted the overlap. The concurrent
test above will look correct whether or not the implementation is. Warm
the pool, instrument the interleaving, and see it go red.

**From M4.3, on comments.** Two of its six findings were lessons this
project had already written down, broken in the unit that quoted them.
When writing why something is correct, check that it is the reason.

### Check when done

- `POST /v1/workspaces/{id}/sessions` with a repeated key returns the
  first session and creates nothing.
- Concurrent duplicates produce one session, proven against real
  PostgreSQL and watched failing first.
- The scope is documented in the tracker, along with the claim mechanism,
  the refusal a concurrent caller receives, the `X-Request-Id` decision,
  and the retention period — each with why it was chosen.
- No existing caller is broken by the header being absent, **and our own
  client sends one**, so the protection is real rather than available.
- `make ci` and `make test-integration` pass.
