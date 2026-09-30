-- name: ListWorkspaceEgressHosts :many
SELECT * FROM workspace_egress_hosts WHERE workspace_id = $1 ORDER BY hostname;

-- name: GetWorkspaceEgressHost :one
SELECT * FROM workspace_egress_hosts WHERE id = $1 AND workspace_id = $2;

-- name: WorkspaceEgressHostExists :one
SELECT EXISTS (SELECT 1 FROM workspace_egress_hosts WHERE workspace_id = $1 AND hostname = $2);

-- name: CountWorkspaceEgressHosts :one
SELECT count(*) FROM workspace_egress_hosts WHERE workspace_id = $1;

-- name: InsertWorkspaceEgressHost :one
INSERT INTO workspace_egress_hosts (id, workspace_id, hostname, created_by)
VALUES ($1, $2, $3, $4) RETURNING *;

-- name: DeleteWorkspaceEgressHost :one
DELETE FROM workspace_egress_hosts WHERE id = $1 AND workspace_id = $2 RETURNING *;

-- name: GetRunnerEgressSnapshot :one
SELECT * FROM runner_egress_snapshots WHERE runner_id = $1 AND workspace_id = $2;
