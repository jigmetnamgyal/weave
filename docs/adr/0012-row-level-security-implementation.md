# ADR-012: Row-level security, and the three things that make it real

- Status: Accepted
- Date: 2026-09-11
- Updates: ADR-010 (which deferred this)

## Context

ADR-010 chose workspace-scoped queries as the primary tenant control and
deferred row-level security, naming a hard gate: RLS before the first external
customer. It shipped earlier than that gate, in M3.0, because M3.1 roughly
doubles the tenant-owned surface and four tables are cheaper to convert than
eight.

RLS is easy to add and easy to add uselessly. This records the three decisions
that separate the two.

## Decision

**1. The application connects as a role that policies apply to.**

`FORCE ROW LEVEL SECURITY` subjects a table's _owner_ to its policies. It does
nothing about a superuser or a `BYPASSRLS` role, both of which ignore RLS
entirely — and the local `weave` role, created by the postgres image from
`POSTGRES_USER`, is a superuser.

So the load-bearing control is not `FORCE`. It is that the API connects as
`weave_app`, which owns nothing and is neither superuser nor `BYPASSRLS`.
`APP_DATABASE_URL` is deliberately separate from `DATABASE_URL` so that
reverting to the owner's connection is a visible change rather than a
one-character edit. `FORCE` is kept as the second layer, for any environment
whose owner is not a superuser.

**2. Tenant context is transaction-scoped.**

Policies read `app.user_id` and `app.workspace_id`, set with `SET LOCAL` at the
start of every transaction. `SET LOCAL` is discarded at commit or rollback.

This is the sharp edge of RLS with connection pooling: a session-scoped `SET`
would hand one request's tenant to whatever request next borrowed that
connection — producing exactly the cross-tenant read the policies exist to
prevent, introduced by the mechanism meant to prevent it. There is a test that
pins a single connection, sets context in one transaction, commits, and asserts
the next transaction on that same connection sees nothing.

A consequence worth stating: **every tenant read now runs inside a
transaction**, not only writes. Several previously did not.

**3. Absent context denies.**

The context helpers return NULL when a setting is missing, and a policy
comparing against NULL matches no row. A query that reaches the database
without context returns nothing.

RLS that fails open would be worse than no RLS: it would look like protection
while providing none, and the failure would be invisible until a tenant read
another tenant's data.

## The deliberate holes

Two operations genuinely cannot be workspace-scoped, and each is a named
function rather than a blanket exemption:

- **`weave_is_member(uuid)`** — `SECURITY DEFINER`, because a policy on
  `workspace_members` that queries `workspace_members` recurses. It answers
  only about the current user, so it discloses nothing.
- **`weave_invitation_by_token(bytea)`** — `SECURITY DEFINER`, returning
  exactly the row whose token hash was presented. Accepting an invitation is
  the one operation available to someone in no workspace; the token is the
  authorization. Keep this to one function. If a second such function is ever
  proposed, that is the moment to ask whether the model is still right.

## Consequences

**Accepted:**

- Accepting an invitation now inserts the membership _before_ claiming the
  invitation, because the invitation and audit policies authorise through
  `weave_is_member`, which only becomes true once that row exists. Single use
  is unaffected — the claim is still conditional, and the membership primary
  key settles concurrent accepts just as firmly.
- Existing integration tests connect as the owner, which is a superuser
  locally, so they bypass RLS and test the application's own filtering rather
  than the policies. The RLS tests connect as `weave_app` and refuse to run at
  all if the role can bypass — a test that silently ran as superuser would pass
  while proving nothing.
- Two pools in the API: the readiness probe uses the owner so a policy
  misconfiguration cannot make the service look unhealthy, and every request
  uses the application role. Readiness also probes the application pool and
  asserts that the role it connects as is neither superuser nor `BYPASSRLS` —
  the one control the unit rests on, checked continuously rather than assumed
  from configuration. It rides on readiness rather than startup because the
  service is required to start while a dependency is down; a misconfigured role
  means the instance never becomes ready, and so never takes traffic.

**Deployment ordering:** ship the configuration change pointing the API at
`APP_DATABASE_URL` first, then apply the migration. Where the owner is a
superuser the previous version survives either order, but that is a property of
one deployment and not something to depend on.

**Not covered:** `users` carries no `workspace_id` and has no policy. A user
exists independently of any workspace, and provisioning happens before
membership.
