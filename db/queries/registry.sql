-- name: InsertRegistryRequest :exec
-- Recorded before the proxy forwards the request. Runs in the tenant context
-- of the runner's workspace; RLS refuses any other.
INSERT INTO registry_requests (id, workspace_id, session_id, runner_id, host, method, path)
VALUES (
    sqlc.arg(id), sqlc.arg(workspace_id), sqlc.arg(session_id), sqlc.arg(runner_id),
    sqlc.arg(host), sqlc.arg(method), sqlc.arg(path)
);

-- name: ListRegistryRequests :many
-- A session's registry requests, oldest first. Tenant-scoped.
SELECT id, workspace_id, session_id, runner_id, host, method, path, requested_at
FROM registry_requests
WHERE session_id = sqlc.arg(session_id) AND workspace_id = sqlc.arg(workspace_id)
ORDER BY requested_at, id;
