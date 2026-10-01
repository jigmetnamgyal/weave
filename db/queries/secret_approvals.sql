-- name: InsertSecretReservation :exec
INSERT INTO provider_secret_reservations(project_number,secret_id,environment) VALUES($1,$2,$3);

-- name: InsertSecretIntent :exec
INSERT INTO provider_secret_intents(id,workspace_id,provider,environment,project_number,secret_id,initial_version,principal_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$8);

-- name: GetSecretIntent :one
SELECT * FROM provider_secret_intents WHERE id=$1 AND workspace_id=$2;

-- name: InsertSecretAssignment :exec
INSERT INTO provider_secret_assignments(intent_id,workspace_id,secret_seconds,secret_nanos,principal_id) VALUES($1,$2,$3,$4,$5);

-- name: GetSecretAssignment :one
SELECT * FROM provider_secret_assignments WHERE intent_id=$1 AND workspace_id=$2;

-- name: InsertSecretApproval :exec
INSERT INTO provider_secret_approvals(id,intent_id,workspace_id,secret_version,version_seconds,version_nanos,principal_id)
VALUES($1,$2,$3,$4,$5,$6,$7);

-- name: GetSecretApprovalDecision :one
SELECT * FROM provider_secret_approvals WHERE id=$1 AND workspace_id=$2;

-- name: InsertSecretWithdrawal :exec
INSERT INTO provider_secret_withdrawals(id,intent_id,workspace_id,approval_id,principal_id) VALUES($1,$2,$3,$4,$5);

-- name: GetSecretWithdrawal :one
SELECT * FROM provider_secret_withdrawals WHERE id=$1 AND workspace_id=$2;

-- name: LockSecretIntentRoot :one
SELECT r.state FROM provider_secret_reservations r
JOIN provider_secret_intents i ON i.project_number=r.project_number AND i.secret_id=r.secret_id
WHERE i.id=$1 AND i.workspace_id=$2 FOR UPDATE OF r;

-- name: LookupSecretApproval :one
SELECT a.id,a.schema_version,a.credential_kind,a.version_seconds,a.version_nanos,
 i.workspace_id,i.provider,i.environment,i.project_number,i.secret_id,i.resource_family,
 s.secret_seconds,s.secret_nanos
FROM provider_secret_approvals a
JOIN provider_secret_intents i ON i.id=a.intent_id AND i.workspace_id=a.workspace_id
JOIN provider_secret_assignments s ON s.intent_id=a.intent_id AND s.workspace_id=a.workspace_id
WHERE i.workspace_id=$1 AND i.provider=$2 AND i.environment=$3 AND i.project_number=$4 AND i.secret_id=$5
 AND a.secret_version=$6
 AND NOT EXISTS(SELECT 1 FROM provider_secret_withdrawals w WHERE w.intent_id=a.intent_id AND w.workspace_id=a.workspace_id AND (w.approval_id IS NULL OR w.approval_id=a.id));

-- name: LookupSelectedSecretApproval :one
SELECT a.id,a.schema_version,a.credential_kind,a.version_seconds,a.version_nanos,
 i.workspace_id,i.provider,i.environment,i.project_number,i.secret_id,i.resource_family,
 s.secret_seconds,s.secret_nanos
FROM provider_secret_approvals a
JOIN provider_secret_intents i ON i.id=a.intent_id AND i.workspace_id=a.workspace_id
JOIN provider_secret_assignments s ON s.intent_id=a.intent_id AND s.workspace_id=a.workspace_id
WHERE a.id=$1 AND i.workspace_id=$2 AND i.provider=$3 AND i.environment=$4 AND i.project_number=$5 AND i.secret_id=$6
 AND a.secret_version=$7
 AND NOT EXISTS(SELECT 1 FROM provider_secret_withdrawals w WHERE w.intent_id=a.intent_id AND w.workspace_id=a.workspace_id AND (w.approval_id IS NULL OR w.approval_id=a.id));
