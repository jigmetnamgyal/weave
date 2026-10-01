-- name: GetProviderCredentialBinding :one
SELECT * FROM workspace_provider_credentials WHERE workspace_id=$1 AND provider=$2;

-- name: InsertProviderCredentialBinding :one
INSERT INTO workspace_provider_credentials(id,workspace_id,provider,created_by)
VALUES($1,$2,$3,$4) RETURNING *;

-- name: InsertProviderCredentialResource :one
INSERT INTO provider_credential_resources(id,workspace_id,credential_id,environment,project_number,secret_id)
VALUES($1,$2,$3,$4,$5,$6)
ON CONFLICT(project_number,secret_id) DO NOTHING RETURNING *;

-- name: GetOwnedCredentialResource :one
SELECT * FROM provider_credential_resources
WHERE project_number=$1 AND secret_id=$2 AND workspace_id=$3 AND credential_id=$4;

-- name: InsertProviderCredentialVersion :exec
INSERT INTO provider_credential_versions(id,workspace_id,credential_id,resource_id,secret_version,registration_epoch,verification_id,created_by)
VALUES($1,$2,$3,$4,$5,$6,$7,$8);

-- name: ActivateProviderCredential :one
UPDATE workspace_provider_credentials SET state='active',epoch=epoch+1,current_version_id=$4,updated_at=now()
WHERE workspace_id=$1 AND provider=$2 AND epoch=$3 RETURNING *;

-- name: DisableProviderCredential :one
UPDATE workspace_provider_credentials SET state='revoked',epoch=epoch+1,updated_at=now()
WHERE workspace_id=$1 AND provider=$2 AND epoch=$3 AND state='active' RETURNING *;
