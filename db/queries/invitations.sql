-- name: CreateInvitation :one
INSERT INTO workspace_invitations (id, workspace_id, email, role, token_hash, invited_by, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetOutstandingInvitationForEmail :one
-- The row occupying the one-outstanding slot, if any. May be expired: the
-- unique index predicate cannot reference now(), so the caller decides.
SELECT * FROM workspace_invitations
WHERE workspace_id = $1 AND email = $2
  AND accepted_at IS NULL AND revoked_at IS NULL;

-- name: GetInvitationByTokenHash :one
-- Deliberately unscoped by workspace: the token is the only thing the
-- acceptor holds, and they are not yet a member of anything. The caller
-- checks status and email before acting on the result.
SELECT * FROM workspace_invitations
WHERE token_hash = $1;

-- name: GetInvitationForWorkspace :one
-- Scoped by workspace so an invitation id from one tenant cannot be revoked
-- through another.
SELECT * FROM workspace_invitations
WHERE id = $1 AND workspace_id = $2;

-- name: ListInvitationsForWorkspace :many
SELECT i.*, u.email AS invited_by_email, u.display_name AS invited_by_display_name
FROM workspace_invitations i
JOIN users u ON u.id = i.invited_by
WHERE i.workspace_id = $1
ORDER BY i.created_at DESC;

-- name: RevokeInvitation :one
-- Only an outstanding invitation can be revoked; revoking an accepted or
-- already-revoked one matches no row.
UPDATE workspace_invitations
SET revoked_at = now(), revoked_by = $3
WHERE id = $1 AND workspace_id = $2
  AND accepted_at IS NULL AND revoked_at IS NULL
RETURNING *;

-- name: ClaimInvitation :one
-- Single-statement claim: the WHERE clause is what makes the token single
-- use. Two concurrent accepts race here and exactly one matches a row, so no
-- lock is needed and no second membership can be created.
UPDATE workspace_invitations
SET accepted_at = now(), accepted_by = $2
WHERE id = $1
  AND accepted_at IS NULL
  AND revoked_at IS NULL
  AND expires_at > now()
RETURNING *;

-- name: GetInvitationWorkspaceContext :one
-- Backing for the preview: the workspace name and who invited you, and
-- nothing else about either.
SELECT w.name AS workspace_name,
       u.email AS invited_by_email,
       u.display_name AS invited_by_display_name
FROM workspace_invitations i
JOIN workspaces w ON w.id = i.workspace_id
JOIN users u ON u.id = i.invited_by
WHERE i.id = $1;
