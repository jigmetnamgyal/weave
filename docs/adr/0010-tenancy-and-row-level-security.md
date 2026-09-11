# ADR-010: Tenancy enforced by workspace-scoped queries, with row-level security deferred

- Status: Accepted; **superseded in part by ADR-012 (2026-09-11), which records that row-level security shipped**
- Date: 2026-09-10
- Supersedes: nothing
- Related: ADR-002 (PostgreSQL as source of truth), ADR-009 (Clerk for
  authentication only)

## Context

Weave is multi-tenant. `architecture.md` states the invariant plainly: _every
tenant-owned data access is scoped by a verified workspace ID and authorization
decision_, and a cross-tenant read is the most serious defect this product can
ship. It also calls for PostgreSQL row-level security "as defense in depth for
high-risk tables, with the application setting the verified tenant context per
transaction."

Unit M2.2 introduces the first tenant-owned tables, so this is the moment the
strategy has to be decided rather than assumed.

Three mechanisms were available:

1. **Workspace-scoped queries.** Every store method takes a workspace ID, and
   every query filters on `workspace_id` even where the primary key is already
   unique. Enforced by code review and tests.
2. **Row-level security.** PostgreSQL policies reject cross-tenant rows in the
   database, regardless of what the query asked for. Requires the application
   to connect as a non-superuser role and to set a verified tenant identifier
   per transaction.
3. **A separate schema or database per tenant.** Strongest isolation, and
   incompatible with the cost structure of a product whose customers are teams
   of three to twenty developers.

## Decision

**Workspace-scoped queries are the primary control, implemented now.
Row-level security is deferred to a dedicated unit. Schema-per-tenant is
rejected outright.**

Concretely, as of M2.2:

- No store method for tenant-owned data exists without a workspace ID
  parameter. There is no unscoped `GetByID`.
- Reads join through `workspace_members`, so a caller who is not a member gets
  no row — "not found" and "not yours" are indistinguishable at every layer,
  including the HTTP status.
- Authorization is two steps: membership decides visibility (404 when absent),
  and the permission matrix decides the operation (403 when denied).
- Integration tests assert cross-tenant reads return nothing, using real
  PostgreSQL rather than a fake.

## Update, 2026-09-11: row-level security shipped

RLS landed in M3.0, ahead of any of the triggers below firing. The deferral
reasoning stood — none of the three conditions had been met — but the tenant
surface was about to roughly double with M3.1, and four tables are cheaper to
convert than eight. See ADR-012 for what was built.

One correction to what this ADR implied. It described RLS as protecting
against an application running as owner; that is only half right. `FORCE ROW
LEVEL SECURITY` subjects a table's _owner_ to its policies, but a **superuser
or a `BYPASSRLS` role ignores RLS regardless**, and the local `weave` role is a
superuser. The control that actually carries isolation is the application
connecting as `weave_app`, which is none of those three. `FORCE` is the second
layer, for environments whose owner is not a superuser.

The rest of this ADR is retained as the record of why the deferral was taken.

## Why row-level security was deferred, and what brought it forward

Deferring a security control deserves an explicit reason, so:

- RLS is **defence in depth**, not the primary control. `architecture.md`
  positions it that way. Adding it to a codebase whose queries are not already
  scoped would be papering over the real problem; adding it after they are
  scoped is a genuine second layer.
- It is not a small change. It needs a non-superuser application role, a
  per-transaction `SET LOCAL` of a verified tenant identifier, a policy per
  table, and a migration path — plus care that connection pooling never leaks
  a tenant context between checkouts, which is a live footgun with pgxpool.
- Bundling it into M2.2 would have produced a unit too large to review
  carefully, and this is precisely the unit that most deserves careful review.

It should be brought forward when **any** of these becomes true:

1. A second tenant-owned table is added by someone other than the author of
   this ADR — the convention's durability is then load-bearing on people who
   did not set it.
2. Raw SQL appears anywhere outside `db/queries/`, since sqlc's generated
   accessors are currently what makes the convention mechanically checkable.
3. Before the first production tenant that is not the Weave team itself.

Item 3 is the hard gate: **RLS ships before external customers.**

## Consequences

**Accepted:**

- Tenant isolation currently rests on a convention plus tests, not on a
  mechanism the database enforces. A query written without a workspace filter
  would compile, pass review if nobody noticed, and leak.
- That risk is bounded while the schema is small and every query lives in one
  reviewed file.

**Mitigations in place:**

- `db/queries/` is a single directory; every tenant query is visible in one
  place during review.
- Integration tests prove cross-tenant reads return nothing, and prove it
  against real PostgreSQL.
- Membership-joined reads mean the common failure — forgetting a filter — most
  often produces no row rather than another tenant's row.

**Rejected alternative — schema per tenant:** strongest isolation, but
migrations become O(tenants), connection pooling fragments, and cross-tenant
operational queries become impractical. The ICP is small teams; the economics
do not support it.
