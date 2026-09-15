-- name: CreateAgent :one
INSERT INTO agents (id, workspace_id, name, created_by)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: SetAgentCurrentVersion :one
-- The pointer is the only mutable thing about a profile. Moving it changes
-- what the next session will use and nothing about what past sessions did.
UPDATE agents
SET current_version_id = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: RenameAgent :one
UPDATE agents
SET name = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: GetAgentForWorkspace :one
SELECT * FROM agents
WHERE id = $1 AND workspace_id = $2;

-- name: ListAgentsForWorkspace :many
SELECT * FROM agents
WHERE workspace_id = $1
ORDER BY name;

-- name: CreateAgentVersion :one
-- Append-only: there is no update or delete for this table, and the triggers
-- refuse both. Editing a profile means writing another row.
INSERT INTO agent_versions (
    id, agent_id, workspace_id, version, provider, model, capabilities, tool_policy, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: NextAgentVersionNumber :one
-- Read under the agent's row lock by the caller, so two concurrent edits
-- cannot both compute the same next number and collide on the unique index.
SELECT COALESCE(MAX(version), 0) + 1 AS next
FROM agent_versions
WHERE agent_id = $1 AND workspace_id = $2;

-- name: GetAgentVersionForWorkspace :one
SELECT * FROM agent_versions
WHERE id = $1 AND workspace_id = $2;

-- name: ListAgentVersionsForWorkspace :many
SELECT * FROM agent_versions
WHERE agent_id = $1 AND workspace_id = $2
ORDER BY version DESC;

-- name: LockAgentForUpdate :one
-- Taken before computing the next version number. Without it two concurrent
-- edits both read the same MAX(version) and one fails on the unique index —
-- an error the caller can do nothing useful with.
SELECT * FROM agents
WHERE id = $1 AND workspace_id = $2
FOR UPDATE;
