Read `CLAUDE.md` before starting

We're connecting workspaces to GitHub (Unit M3.1): a workspace installs
the Weave GitHub App, chooses which repositories it may reach, and Weave
records that grant well enough to authorize later work against it.

This is the first unit where Weave holds a credential that reaches
something outside itself. Everything before it was internal: a session
token proved who you were, a policy decided what you could do, and the
blast radius of a mistake stopped at our own database. An installation
token reaches a customer's source code. That changes what "get it wrong"
costs, and the decisions below are shaped by it.

## What this unit is not

Scope discipline matters more here than usual, because "GitHub
integration" can absorb an unbounded amount of work.

**In:** installing the App against a workspace, recording which
repositories that installation grants, keeping those records true as the
installation changes, and answering "may this workspace use this
repository?" correctly.

**Out, and deliberately:** cloning, branches, commits, pull requests, and
push or pull-request webhooks. Those are M3.2. A unit that both
establishes access and starts using it would be too large to review
carefully, and this is the half where a mistake is worse.

The test of whether this unit is done is not that anything useful happens
with a repository. It is that the grant is recorded correctly and cannot
be borrowed by another tenant.

## The operator action this depends on

A real GitHub App must exist before any of this can be built, and only
the account owner can create one. Create it at **Settings → Developer
settings → GitHub Apps → New GitHub App** with:

- **Callback URL** — `{APP_URL}/api/github/callback`, "Request user
  authorization (OAuth) during installation" enabled.
- **Setup URL** — `{APP_URL}/github/installed`, "Redirect on update"
  enabled, so repository-selection changes come back to us.
- **Webhook URL** — `{API_URL}/v1/github/webhook`, with a generated
  webhook secret.
- **Repository permissions** — Contents: read and write; Metadata:
  read-only; Pull requests: read and write. Nothing else. Each extra
  permission is one a customer has to grant and we have to justify.
- **Subscribe to events** — Installation target, Repository. Not push or
  pull request: those belong to M3.2, and subscribing early means
  receiving traffic we have no handler for.
- **Where can this App be installed** — any account.

It yields an App ID, a client ID and secret, a webhook secret, and a
generated private key (`.pem`). Add them to `.env.example` as empty keys
and to `.env` with real values.

## Decisions this unit locks in

- **The private key is environment configuration, never database
  content.** It is the root credential for every installation; a
  database dump must not be a set of working App credentials.
- **Installation tokens are cached, never persisted.** GitHub issues them
  for one hour. Redis with a TTL shorter than GitHub's, never Postgres.
- **GitHub is the source of truth for access; our records are a cache.**
  A repository row is what we believe the installation grants. Belief
  goes stale — repositories get deselected, installations suspended or
  deleted. Authorization must be built so a stale row cannot outlive the
  grant it describes.
- **Binding an installation to a workspace is the security boundary of
  this unit.** Everything else is bookkeeping.
- Write `docs/adr/0005-github-app-credentials.md`. ADR-005 is currently a
  row in the tracker with no document behind it, and this is the unit
  that gives it content.

## Binding an installation to a workspace

This is the part to get right.

GitHub sends the user back from installation with an `installation_id`
and nothing else that identifies which workspace they meant. If we take
the workspace from anything the caller controls at that moment, then
whoever holds an `installation_id` can attach it to a workspace they
choose — or attach someone else's installation to their own workspace and
read a repository they were never granted.

So:

- Before redirecting to GitHub, mint a **single-use, expiring state
  token** carrying the workspace id and the acting user id. Store it
  server-side, keyed by a random value; the value is what travels in the
  `state` parameter.
- On callback, look the state up, delete it, and fail closed if it is
  missing, expired or already used. Then re-check that the actor still
  holds `repository:manage` in that workspace — the state proves intent,
  not current authority, and membership can change while a browser sits
  on GitHub's consent screen.
- Verify with GitHub that the `installation_id` is what the callback
  claims, using an App JWT, rather than trusting the query parameter.
- **One installation belongs to exactly one workspace.** If an
  installation is already bound elsewhere, refuse and say so plainly
  rather than rebinding. Silent rebinding would move a repository grant
  between tenants on a single request.

The same reasoning applies to the setup-URL redirect after a repository
selection change: it carries an `installation_id`, so it is a claim, not
a fact.

## Data

Three tables, all workspace-owned, all created with row-level security
in the same migration that creates them:

- `github_installations` — `workspace_id`, GitHub installation id,
  account login and type, selected-repository mode, suspension state,
  timestamps. Unique on the GitHub installation id, which is what makes
  double-binding a constraint rather than a convention.
- `repositories` — `workspace_id`, `installation_id`, GitHub repository
  id, owner, name, default branch, private flag, and whether the
  installation currently grants it.
- `repository_permissions` — the permissions the installation actually
  holds, recorded at connect time and refreshed, so a health check can
  say what is missing rather than only that something is.

Carry over both M3.0 lessons, because they are easy to lose:

- **`ENABLE` and `FORCE ROW LEVEL SECURITY` plus policies belong in the
  same migration as the `CREATE TABLE`.** RLS is off by default, so a
  table added without them is silently unprotected and looks fine.
- **Every read goes through `inTenantTx`.** A read outside a transaction
  has no tenant context and, by design, returns nothing rather than
  everything.

The GitHub repository id is the stable identity, not owner/name — those
change when a repository is renamed or transferred, and a rename must not
present as a different repository.

## Keeping the records true

A repository row that outlives its grant is the failure mode that
matters, so it gets three defences rather than one:

- **Webhooks.** `installation`, `installation_repositories` and
  `repository` events update the records. Verify the signature as HMAC
  SHA-256 over the **raw body**, compared in constant time, before
  parsing — a handler that parses first has already trusted the input.
  Deduplicate on the delivery id; GitHub retries, and a retry must not
  produce a second effect. Store deliveries with enough metadata to
  answer "did we receive it, and what did we do".
- **Reconciliation on use.** Before an operation that depends on a
  repository, confirm the installation still grants it. Webhooks are
  best-effort — they are missed, delayed and replayed — so they are not
  permitted to be the only thing standing between a revoked grant and a
  clone.
- **A health check.** An endpoint reporting, per installation: reachable,
  not suspended, permissions still sufficient, and which repositories are
  currently granted. This is what someone debugging a broken workspace
  will reach for first.

Handle suspension and deletion explicitly. A suspended installation
authorizes nothing but should not lose its records, because
unsuspending must restore the previous state rather than requiring a
fresh install. A deleted installation should mark its repositories
ungranted and keep the rows for audit.

## Authorization

Unchanged in shape from M2.2, and worth restating because there is now a
second axis:

- Membership decides **visibility**: a non-member gets 404 for a
  workspace's installations and repositories, never 403.
- Permission decides the **operation**: `repository:manage` covers
  connecting an installation, changing repository selection and
  disconnecting. Listing repositories needs only membership.
- GitHub's grant decides **reach**: a repository the installation does
  not grant is unavailable regardless of what any member is permitted to
  do.

All three must hold. The third is new, and it is the one that cannot be
answered from our own database alone.

Audit every installation connect, disconnect, suspension change and
repository-selection change, as with every other privileged action.

## Secrets

- The private key, client secret and webhook secret reach the server
  only. Nothing here is a `NEXT_PUBLIC_` value.
- No token, key or secret appears in a log line, an error body, or an
  audit `detail`. Grep the log for the App ID and webhook secret as part
  of verification, as was done for invitation tokens in M2.3.
- Installation tokens are cached under a key naming the installation, with
  a TTL shorter than GitHub's expiry, and are re-minted rather than
  refreshed.

## Tests

The interesting cases are the ones where an attacker supplies something
plausible:

- A callback with a `state` that is unknown, expired, or already used is
  refused — and the three are indistinguishable to the caller.
- A callback whose `state` names a workspace the actor no longer has
  `repository:manage` in is refused.
- An `installation_id` already bound to another workspace is refused, and
  does not rebind.
- A webhook with a wrong signature, an absent signature, or a body
  altered after signing is rejected before parsing.
- The same delivery id twice produces one effect.
- Removing a repository from the installation makes it unavailable, via
  the webhook and independently via reconciliation with the webhook
  never delivered.
- A suspended installation authorizes nothing; unsuspending restores the
  prior state.
- A repository renamed on GitHub stays the same repository locally.
- Cross-tenant: an installation id and a repository id from another
  workspace are both invisible, not forbidden.
- The RLS tests extend to the three new tables, connecting as
  `weave_app`, including one unfiltered query per table.
- GitHub is faked at the HTTP boundary, not mocked at the client
  interface — signature verification and error handling are most of what
  is worth testing, and an interface mock skips both.

### Check when done

- An owner installs the App, selects repositories, and sees them listed
  in the workspace.
- A second workspace cannot see or use the first workspace's
  installation or repositories.
- Deselecting a repository on GitHub removes it from the workspace,
  through the webhook and through reconciliation alone.
- Suspending the installation disables it; unsuspending restores it.
- A viewer sees repositories but cannot connect or disconnect; the API
  refuses them.
- The health check reports a missing permission accurately after one is
  removed on GitHub.
- No secret appears in the log, verified by grep.
- `docs/adr/0005-github-app-credentials.md` exists and records the
  credential model, the token cache, and the installation-binding rule.
- `make ci` and `make test-integration` pass.
