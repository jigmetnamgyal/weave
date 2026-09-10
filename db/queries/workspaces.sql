-- name: CreateWorkspace :one
INSERT INTO workspaces (id, slug, name, created_by)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetWorkspaceForMember :one
-- Scoped by member, not just by id. A caller who is not a member gets no row,
-- so "not found" and "not yours" are indistinguishable from the outside and
-- workspace identifiers cannot be probed.
SELECT w.*
FROM workspaces w
JOIN workspace_members m ON m.workspace_id = w.id
WHERE w.id = $1 AND m.user_id = $2;

-- name: ListWorkspacesForUser :many
SELECT w.*, m.role
FROM workspaces w
JOIN workspace_members m ON m.workspace_id = w.id
WHERE m.user_id = $1
ORDER BY w.created_at;

-- name: RenameWorkspace :one
-- Optimistic concurrency: the caller supplies the version it read. A stale
-- version matches no row, which the store reports as a conflict rather than
-- silently overwriting a concurrent edit.
UPDATE workspaces
SET name = $3,
    version = version + 1,
    updated_at = now()
WHERE id = $1 AND version = $2
RETURNING *;

-- name: LockWorkspace :one
-- Serialises membership changes within a workspace. Taken before any check
-- that counts owners, so two concurrent demotions cannot both observe two
-- owners and both proceed.
SELECT id FROM workspaces WHERE id = $1 FOR UPDATE;

-- name: SlugExists :one
SELECT EXISTS (SELECT 1 FROM workspaces WHERE slug = $1);

-- name: AddWorkspaceMember :one
INSERT INTO workspace_members (workspace_id, user_id, role)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetWorkspaceMember :one
SELECT * FROM workspace_members
WHERE workspace_id = $1 AND user_id = $2;

-- name: ListWorkspaceMembers :many
SELECT m.workspace_id, m.user_id, m.role, m.created_at, m.updated_at,
       u.email, u.display_name, u.avatar_url
FROM workspace_members m
JOIN users u ON u.id = m.user_id
WHERE m.workspace_id = $1
ORDER BY m.created_at;

-- name: UpdateWorkspaceMemberRole :one
UPDATE workspace_members
SET role = $3, updated_at = now()
WHERE workspace_id = $1 AND user_id = $2
RETURNING *;

-- name: DeleteWorkspaceMember :execrows
DELETE FROM workspace_members
WHERE workspace_id = $1 AND user_id = $2;

-- name: CountWorkspaceOwners :one
-- Used under LockWorkspace to enforce that a workspace never loses its last
-- owner.
SELECT count(*) FROM workspace_members
WHERE workspace_id = $1 AND role = 'owner';

-- name: AppendAuditEvent :one
INSERT INTO audit_events (id, workspace_id, actor_user_id, action, target, detail)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListAuditEvents :many
-- Operator and test support.
SELECT * FROM audit_events
WHERE workspace_id = $1
ORDER BY occurred_at DESC, id
LIMIT $2;
