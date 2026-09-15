-- name: CreateTask :one
INSERT INTO tasks (id, workspace_id, repository_id, title, body, status, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetTaskForWorkspace :one
-- Scoped by workspace, so a task id from one tenant cannot be read through
-- another even before the policies are consulted.
SELECT * FROM tasks
WHERE id = $1 AND workspace_id = $2;

-- name: ListTasksForWorkspace :many
SELECT * FROM tasks
WHERE workspace_id = $1
ORDER BY created_at DESC;

-- name: UpdateTask :one
-- The body is written back exactly as given. Nothing here normalises it: the
-- stored value and the returned value must agree, or nobody can tell what is
-- actually stored.
UPDATE tasks
SET repository_id = $3,
    title         = $4,
    body          = $5,
    status        = $6,
    updated_at    = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: LockTaskForUpdate :one
-- Read at the start of a patch, in the transaction that writes it.
--
-- What makes a patch safe is that the read happens here rather than in an
-- earlier transaction: a read-modify-write split across two transactions lets
-- two concurrent edits to different fields both read the same row, the second
-- writing its own field alongside stale copies of the rest and silently
-- undoing the first. That was measured — reproducing the split shape fails the
-- concurrency test every run.
--
-- FOR UPDATE is not what prevents it today. authorizeActor takes LockWorkspace
-- before this runs, which already serializes every mutation in a workspace, so
-- this narrower lock is redundant in the current arrangement — the same
-- relationship LockAgentForUpdate has. It is here so this transaction states
-- the row it depends on rather than relying on the workspace lock staying as
-- coarse as it is.
SELECT * FROM tasks
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;
