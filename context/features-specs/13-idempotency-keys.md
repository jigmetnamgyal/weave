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
- **complete** — the response is stored and replayed verbatim.

The claim and the work must be in one transaction, for the same reason
the outbox row is: a key claimed by a process that then dies must not
leave the key held forever. Decide whether that is a lease with an
expiry, as `outbox_events` uses, or a claim released by the transaction
itself — and say which, and why, in the tracker.

## Scope means scope

A key is not global. The same string from two workspaces, or two users,
or against two different endpoints, must not collide — and a key replayed
against a *different request body* is a caller bug that must be refused
rather than silently answered with the first result.

The scope has to be documented, because the standard says so and because
a scope nobody wrote down is one every future endpoint invents again.
State it explicitly: what the key is scoped by, and what happens when the
same key arrives with a different body.

## Transport

`Idempotency-Key`, a request header, optional. Absent means the mutation
behaves exactly as it does today — this unit must not make an existing
caller's request fail for want of a header it has never sent.

The contract describes it, so the generated client can send it.

## Data

`idempotency_keys`, workspace-owned, with `ENABLE`/`FORCE ROW LEVEL
SECURITY` and policies **in the creating migration**. Fourth time of
saying it; still true.

Records need at least: the key, its scope, what became of it, the stored
response, and enough to expire rows that will never be replayed. Let the
three outcomes above decide the columns rather than copying a shape from
elsewhere.

**Stored responses are customer data.** A replayed `createSession`
response carries a branch name and identifiers; the retention decision
belongs with `audit_events` and the delivery log, not invented here.

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
- The scope is documented in the tracker, along with the claim mechanism
  and why it was chosen.
- No existing caller is broken by the header being absent.
- `make ci` and `make test-integration` pass.
