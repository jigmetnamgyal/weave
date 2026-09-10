Read `CLAUDE.md` before starting

We're adding workspaces and membership (Unit M2.2): a signed-in user
creates a workspace, becomes its owner, and every permission decision
in the product is answered from PostgreSQL against that membership.

This is the unit that makes tenant isolation real. Everything built
after it — repositories, tasks, sessions — is scoped by a workspace ID
that this unit establishes and verifies.

## Decisions this unit locks in

- Authorization is answered in PostgreSQL, never by the identity
  provider. Clerk says who someone is; membership says what they may
  do (ADR-009).
- Permissions are action-oriented and defined in one place. No handler
  and no component compares a role name.
- Deny by default: an operation with no explicit permission is denied,
  and a store method with no workspace scope does not exist.
- Record an ADR for the tenancy strategy, including why row-level
  security is deferred and what would bring it forward.

## Database

Add one migration containing:

- `workspaces` — `id` UUIDv7, `slug` citext unique, `name`,
  `created_by` referencing `users`, `version` integer for optimistic
  concurrency, `created_at`, `updated_at`.
- `workspace_members` — `workspace_id`, `user_id`, `role`,
  `created_at`, `updated_at`; primary key on the pair, and an index on
  `user_id` for "which workspaces am I in".
- `audit_events` — append-only: `id`, `workspace_id`, `actor_user_id`,
  `action`, `target`, `occurred_at`, and a small JSONB detail column.
  No updates, no deletes.

Rules the schema must hold:

- `role` is constrained to `owner`, `admin`, `developer`, `viewer`.
- A workspace always has at least one owner. Removing or demoting the
  last owner fails.
- Deleting a workspace cascades to its members; `audit_events` does
  not cascade, because deleting the thing under audit must not delete
  the record of it.

## Domain and permissions

- `internal/domain` gains `Workspace`, `Membership` and `Role`, plus
  the role-to-permission matrix.
- Permissions are named for actions, following `architecture.md`:
  `workspace:read`, `workspace:manage`, `member:invite`,
  `member:manage`, `session:create`, `session:control`,
  `action:approve`, `repository:manage`, `billing:manage`.
- The matrix is a single table in one file, exhaustive over roles, and
  is the only place a role implies anything.
- Add an exhaustiveness check so a new role or permission cannot be
  added without deciding every pairing.

## Authorization

- Add a workspace-scoped authorization step that runs after
  authentication: resolve the workspace from the route, load the
  caller's membership, and reject with 404 when they are not a member.
- Use 404, not 403, for a workspace the caller cannot see. 403 confirms
  the workspace exists, which leaks tenant structure to anyone probing
  identifiers.
- Every handler then asserts a specific permission. Membership alone
  authorizes nothing.
- Evaluate permission at the moment of the decision, never from a
  cached claim.

## Data access

- Every store method for tenant-owned data takes a workspace ID as an
  explicit parameter. There is no unscoped `GetByID`.
- Queries filter on `workspace_id` even when the primary key is
  already unique — a compromised or mistaken identifier must not be
  enough to read another tenant's row.

## API

- `POST /v1/workspaces` — create; the caller becomes owner.
- `GET /v1/workspaces` — the caller's workspaces.
- `GET /v1/workspaces/{id}` — one workspace.
- `PATCH /v1/workspaces/{id}` — rename; requires `workspace:manage`
  and honours the `version` column.
- `GET /v1/workspaces/{id}/members` — list members.
- `PATCH /v1/workspaces/{id}/members/{userId}` — change role;
  requires `member:manage`.
- `DELETE /v1/workspaces/{id}/members/{userId}` — remove a member;
  requires `member:manage`. Removing yourself is allowed unless you
  are the last owner.
- Append an `audit_events` row for every workspace creation, role
  change and member removal, in the same transaction as the change.
- Update `contracts/openapi/openapi.yaml` alongside the handlers.

## Web

- After sign-in, a user with no workspace is asked to create one.
- A user with workspaces sees them listed and can open one.
- Show the current workspace and the caller's role in the header.
- Use the existing theme tokens and shadcn primitives. Do not edit
  `components/ui/*`.

## Tests

The authorization matrix is the point of this unit, so test it
directly and exhaustively:

- Every role against every permission, as a table.
- Non-member, removed member, unauthenticated, and a member of a
  *different* workspace, for every endpoint.
- Cross-workspace access returns 404 and never 403.
- Last-owner protection: cannot demote, cannot remove, cannot leave.
- Optimistic concurrency: a stale `version` on rename is rejected.
- Audit rows are written in the same transaction as the change they
  describe, and a failed change writes none.
- An integration test proving a store method cannot return a row from
  another workspace.

### Check when done

- A new user signs in, is prompted to create a workspace, and lands in
  it as owner.
- The workspace and the caller's role appear in the header.
- A member of another workspace gets 404 on every endpoint for a
  workspace they do not belong to.
- A viewer is refused every management operation; an owner is refused
  none.
- The last owner cannot be demoted, removed, or leave.
- Every workspace creation, role change and member removal leaves an
  `audit_events` row naming the actor.
- `make ci` and `make test-integration` pass.
