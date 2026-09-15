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
