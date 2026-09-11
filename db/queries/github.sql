-- name: ConnectInstallation :one
-- Bind an installation to a workspace.
--
-- No ON CONFLICT clause on purpose. The unique index on
-- github_installation_id is what refuses a second binding, and the refusal
-- must reach the caller as an error rather than be absorbed into an update:
-- quietly rebinding would move a repository grant between tenants.
INSERT INTO github_installations (
    id, workspace_id, github_installation_id, account_login, account_type,
    repository_selection, connected_by
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetInstallationForWorkspace :one
-- Scoped by workspace, so an installation id from one tenant cannot be read
-- through another even before the policies are consulted.
SELECT * FROM github_installations
WHERE id = $1 AND workspace_id = $2;

-- name: GetInstallationByGitHubIDForWorkspace :one
SELECT * FROM github_installations
WHERE github_installation_id = $1 AND workspace_id = $2;



-- name: SetInstallationSuspended :one
-- Suspension is recorded, never deleted. Unsuspending must restore the
-- previous state rather than require a fresh install, and keeping the rows is
-- what makes that possible.
UPDATE github_installations
SET suspended_at = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: SetInstallationSelection :one
UPDATE github_installations
SET repository_selection = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2
RETURNING *;

-- name: MarkInstallationDeleted :exec
-- Used when GitHub reports the installation removed.
--
-- The row is marked, not deleted. Deleting it cascades to the repositories,
-- and those rows are the record that access once existed — which is what makes
-- an old audit entry or a finished session readable. Withdrawing the
-- repositories is a separate statement in the same transaction.
UPDATE github_installations
SET deleted_at = now(), suspended_at = COALESCE(suspended_at, now()), updated_at = now()
WHERE id = $1 AND workspace_id = $2;

-- name: ListActiveInstallationsForWorkspace :many
-- What the workspace currently has connected. Removed installations are kept
-- for their repository history but are not connections any more.
SELECT * FROM github_installations
WHERE workspace_id = $1 AND deleted_at IS NULL
ORDER BY created_at;

-- name: UpsertRepository :one
-- Reconciliation writes every repository GitHub currently grants.
--
-- ON CONFLICT updates rather than inserts because a repository seen before may
-- have been renamed, transferred, made private, or re-granted after being
-- withdrawn. The conflict target is (installation_id, github_repository_id):
-- the GitHub id is the identity, and owner/name are mutable descriptions of it.
INSERT INTO repositories (
    id, workspace_id, installation_id, github_repository_id,
    owner, name, default_branch, private, granted
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, true)
ON CONFLICT (installation_id, github_repository_id) DO UPDATE
SET owner          = EXCLUDED.owner,
    name           = EXCLUDED.name,
    default_branch = EXCLUDED.default_branch,
    private        = EXCLUDED.private,
    granted        = true,
    updated_at     = now()
RETURNING *;

-- name: WithdrawRepositoriesNotIn :exec
-- Mark everything the installation no longer grants.
--
-- Rows are withdrawn rather than deleted: the record that access once existed
-- is what makes an old audit entry or session readable later. Passing the
-- currently granted set and negating it, rather than deleting named ones,
-- means a repository that vanished without an event is still caught.
UPDATE repositories
SET granted = false, updated_at = now()
WHERE installation_id = $1
  AND workspace_id = $2
  AND granted = true
  AND NOT (github_repository_id = ANY(@granted_ids::bigint[]));

-- name: WithdrawAllRepositories :exec
UPDATE repositories
SET granted = false, updated_at = now()
WHERE installation_id = $1 AND workspace_id = $2 AND granted = true;

-- name: ListRepositoriesForWorkspace :many
-- Withdrawn repositories are returned too, with granted = false, so the
-- interface can say "access was removed" rather than silently dropping a
-- repository someone was using yesterday.
SELECT * FROM repositories
WHERE workspace_id = $1
ORDER BY owner, name;

-- name: GetRepositoryForWorkspace :one
SELECT * FROM repositories
WHERE id = $1 AND workspace_id = $2;

-- name: ClearInstallationPermissions :exec
DELETE FROM repository_permissions
WHERE installation_id = $1 AND workspace_id = $2;

-- name: RecordInstallationPermission :exec
INSERT INTO repository_permissions (installation_id, workspace_id, permission, access)
VALUES ($1, $2, $3, $4)
ON CONFLICT (installation_id, permission) DO UPDATE
SET access = EXCLUDED.access, recorded_at = now();

-- name: ListInstallationPermissions :many
SELECT * FROM repository_permissions
WHERE installation_id = $1 AND workspace_id = $2
ORDER BY permission;

-- name: RecordWebhookDelivery :one
-- Deduplication. GitHub retries deliveries, and a retry must not produce a
-- second effect.
--
-- ON CONFLICT DO NOTHING with a RETURNING clause yields no row when the
-- delivery has been seen, which is how the caller distinguishes the two
-- without a separate read and the race that would come with it.
INSERT INTO github_webhook_deliveries (delivery_id, event, action)
VALUES ($1, $2, $3)
ON CONFLICT (delivery_id) DO NOTHING
RETURNING *;

-- name: PruneWebhookDeliveries :exec
-- Deliveries are only needed while deduplication might see a retry. GitHub
-- gives up well inside this window.
DELETE FROM github_webhook_deliveries
WHERE received_at < now() - @retention::interval;
