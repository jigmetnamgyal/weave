Read `CLAUDE.md` before starting

We're adding PostgreSQL row-level security (Unit M3.0): the database
itself refuses to return another tenant's rows, regardless of what the
query asked for.

Today tenant isolation rests on a convention — every store method takes a
workspace ID, every query filters on it — plus tests. That convention
holds, but a query written without the filter would compile, pass review
if nobody noticed, and leak. This unit makes the database the second
line, so that a mistake in the first one is caught rather than shipped.

ADR-010 deferred this and named a hard gate: RLS before the first
external customer. It is being done now instead, while there are four
tenant-owned tables rather than eight.

## Decisions this unit locks in

- **Two roles.** Migrations and operations connect as the table owner.
  The API connects as a separate, non-owning role that policies apply
  to. An owner bypasses RLS by default, so an application running as
  owner would have policies that silently do nothing.
- **Tenant context is transaction-scoped**, set with `SET LOCAL`. It is
  discarded at commit or rollback, so a pooled connection cannot carry
  one request's tenant into the next.
- **Absent context denies.** A query that runs without the context set
  returns nothing rather than everything. RLS must fail closed, or it is
  worse than no RLS at all — it looks like protection and is not.
- Update ADR-010 to record that this shipped, and what changed.

## Roles and connection

- Add a `weave_app` role: `LOGIN`, no `BYPASSRLS`, not the owner of any
  table, granted only `SELECT, INSERT, UPDATE, DELETE` on the tables it
  needs and `USAGE` on sequences.
- The API connects as `weave_app`. Migrations continue to connect as the
  owner.
- Add a separate `APP_DATABASE_URL` alongside `DATABASE_URL`, both in
  `.env.example` and in the API's required configuration. Keeping them
  distinct is what stops a future change quietly reverting to the
  owner's connection.
- The local Compose stack creates the role on first start.

## Tenant context

- Every request that touches tenant-owned data opens a transaction and
  sets the context before its first statement:
  - `app.user_id` — always, for an authenticated request.
  - `app.workspace_id` — when the operation is scoped to one workspace.
- Use `SET LOCAL`, never `SET`. `SET` persists for the session, and a
  pooled connection outlives the request.
- Wrap this in one helper that opens the transaction, sets the context
  and runs the body. Store methods use the helper; nothing sets the
  context by hand.

## Policies

Enable and **force** row-level security on `workspaces`,
`workspace_members`, `workspace_invitations` and `audit_events`.
`FORCE` matters: without it the owner still bypasses, and a future
change that runs the application as owner would disable every policy
without failing anything.

Each policy reads the context through a helper that returns NULL when
the setting is absent, so an unset context matches no row.

- `workspaces` — visible when `id` is the current workspace, or, when no
  workspace is in context, when the current user is a member.
- `workspace_members`, `workspace_invitations`, `audit_events` — visible
  when `workspace_id` is the current workspace.
- `users` is not tenant-owned and gets no policy.

## The operations that cannot be workspace-scoped

Three existing paths are legitimately unscoped, and each needs a
deliberate answer rather than a blanket exemption:

- **`ListWorkspacesForUser`** — spans every workspace the user belongs
  to, so no single workspace context applies. Answered by the
  user-context arm of the `workspaces` policy.
- **`GetInvitationByTokenHash`** — the acceptor is not a member of
  anything yet; the token is the authorization. This needs to read a row
  in a workspace the caller has no context for.
- **User provisioning and `/v1/me`** — `users` carries no
  `workspace_id`.

For the invitation lookup, use a `SECURITY DEFINER` function owned by
the table owner, returning only that one row by token hash. It is a
deliberate, named hole: keep it to a single function, document why, and
do not let it grow into a general escape hatch.

Membership checks inside a policy must also go through a
`SECURITY DEFINER` function, or a policy on `workspace_members` that
queries `workspace_members` will recurse.

## Tests

The point of this unit is that isolation survives a mistake in the
application, so test it by making that mistake on purpose:

- A query with **no** `workspace_id` filter at all returns only the
  current tenant's rows. This is the test that proves RLS is doing
  something the convention was not.
- A query naming another tenant's `workspace_id` explicitly returns
  nothing.
- With no context set, every tenant table returns nothing — including
  after a transaction that did set one, proving `SET LOCAL` was
  discarded.
- Two transactions on the same pooled connection, with different tenant
  contexts, cannot see each other's rows.
- Writes are constrained too: inserting a row for another tenant fails.
- The `SECURITY DEFINER` invitation lookup returns exactly one row and
  cannot be used to read anything else.
- Every existing test still passes unchanged — the convention and the
  policies must agree, and a disagreement means one of them is wrong.

## Migration safety

- The migration must be deployable while the previous application
  version is still running. That version connects as the owner, which
  `FORCE` would newly subject to policies, so ordering matters: create
  the role and grant it first, ship the application change that uses it,
  then enable and force policies.
- State the ordering in the migration's comments and in the tracker. If
  it cannot be made safe in one step, split it and say so.

### Check when done

- The API connects as `weave_app` and every existing flow still works:
  create a workspace, invite, accept, list members.
- A store method with its workspace filter deliberately removed returns
  only the current tenant's rows.
- With no tenant context, tenant tables return nothing.
- Migrations still run as the owner.
- ADR-010 is updated to record that RLS shipped and what it covers.
- `make ci` and `make test-integration` pass.
