# ADR-005: GitHub App credentials, and binding an installation to a tenant

- Status: Accepted
- Date: 2026-09-11 (decision taken during M0; recorded here when M3.1 gave it
  content)

## Context

Weave reaches customer source code. Everything before M3.1 was internal: a
session token proved who you were, a policy decided what you could do, and the
blast radius of a mistake stopped at our own database. An installation token
does not stop there.

This ADR existed as a one-line entry — "use GitHub App credentials, for
repository-scoped revocable access without long-lived personal tokens" — with
no document behind it. That line was right and insufficient. The reasoning that
matters is not App-versus-PAT; it is what the App's own credentials are allowed
to touch, and how an installation gets attached to a tenant.

## Decision

**A GitHub App, not personal access tokens.** A PAT carries the access of the
person who made it, lives until revoked, and is invisible to the organisation
that granted it. An App installation is granted by the account, limited to the
repositories that account selects, revocable from the account's own settings,
and auditable there. The difference that matters most is the last one: the
customer can see and withdraw what we hold without asking us.

**The private key is environment configuration, never database content.** It is
the root credential for every installation — anyone holding it can mint tokens
for every workspace's repositories. It is referenced by path rather than
inlined in the environment, so it stays out of process listings and out of
anything that dumps configuration, and it is read once at startup. Reading at
startup rather than lazily is deliberate: a missing or malformed key is a
misconfiguration, and the moment to discover it is deployment, not the first
time a customer clicks Connect.

**Installation tokens are cached, never persisted.** GitHub issues them for an
hour. They live in Redis with margin against that expiry, and the cache refuses
to store one without a TTL. PostgreSQL would keep them long past their
usefulness and turn a database backup into a set of working repository
credentials.

**No OAuth client secret.** The App does not request user authorization during
installation, so the OAuth credentials GitHub issues to every App are never
used. None is generated and none is configured. An unused credential is still
one that can leak.

**Binding an installation to a workspace is authorized by state we recorded,
not by anything the callback carries.**

This is the decision the rest of the unit is arranged around. GitHub returns an
`installation_id` and nothing identifying which workspace the person meant. If
the workspace came from anything the caller controls at that moment, then
whoever holds an installation id could attach it to a workspace of their
choosing — or attach someone else's installation to their own workspace and
read repositories they were never granted.

So:

- A single-use, expiring value is recorded server-side **before** the redirect,
  carrying the workspace and the acting user, and travels in the `state`
  parameter GitHub preserves across install and update.
- It is consumed with a single `GETDEL`, not a read and a delete. Two requests
  replaying one callback concurrently must not both succeed, and read-then-
  delete leaves exactly that window open.
- Permission is re-checked on the way back, inside the same transaction as the
  insert. The state proves intent, not current authority, and a browser can sit
  on GitHub's consent screen long enough for a demotion.
- The installation id is confirmed with GitHub before anything is written,
  which turns a claim in a query string into a fact.
- One installation belongs to exactly one workspace, enforced by a unique index
  across the whole table rather than a prior read — which is what makes the
  refusal race-free.

**GitHub is the source of truth for access; our records are a cache.** A
repository row is what we believe the installation grants, and belief goes
stale: repositories get deselected, installations suspended or deleted. A stale
row that outlives its grant is the failure mode that matters, so it gets three
defences rather than one — webhooks, reconciliation before use, and a health
check. Webhooks are not permitted to be the only one, because they are missed,
delayed and replayed.

## Consequences

**Accepted:**

- Every tenant-owned GitHub table carries `ENABLE`/`FORCE ROW LEVEL SECURITY`
  and its policies in the same migration that creates it, per ADR-012.
- Resolving an installation id to a workspace needs a fourth `SECURITY DEFINER`
  function, because a webhook has no user and no workspace. It differs from the
  token-keyed ones: a token authorizes itself, whereas an installation id is a
  small integer anyone could guess. What authorizes that call is the HMAC on
  the delivery, verified before the handler reaches the database — the rule
  keeping it narrow is the caller, not the argument.
- Webhook signatures are verified over the raw body before parsing, using the
  SHA-256 header only. GitHub still sends an HMAC-SHA1 header for old
  integrations; honouring it would let a caller downgrade verification to a
  broken hash.
- Reconciliation before use costs a GitHub call on the path of anything that
  touches a repository. That is the price of not letting a revoked grant reach
  a clone, and it is worth paying.

**Deliberately deferred to M3.2:** cloning, branches, commits, pull requests,
and the push and pull-request webhooks. The App is not subscribed to those
events, and should not be until there is something to handle them — in
development the webhook URL is a public smee.io channel, and a push payload
carries commit messages, author email addresses and file paths.

**Revisit if:** a second product surface needs the App's private key, or an
installation ever needs to serve more than one workspace. Both would break
assumptions this ADR treats as settled.
