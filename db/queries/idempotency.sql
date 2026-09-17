-- name: ClaimIdempotencyKey :one
-- The claim, as one statement rather than a check followed by an insert.
--
-- ON CONFLICT DO UPDATE rather than DO NOTHING, because the conflict has two
-- meanings and only the update can tell them apart while taking the lease: a
-- row whose lease has expired and which never completed is a claim whose
-- holder died, and it is claimable again; anything else is either in flight or
-- complete and must not be disturbed.
--
-- The WHERE on the update is what makes that safe. When it does not match,
-- nothing is written and nothing is returned, and the caller reads the row to
-- find out which of the two it is.
INSERT INTO idempotency_keys (
    id, workspace_id, user_id, endpoint, idempotency_key,
    request_fingerprint, leased_until
)
VALUES ($1, $2, $3, $4, $5, $6, now() + @lease::interval)
ON CONFLICT (workspace_id, user_id, endpoint, idempotency_key) DO UPDATE
SET leased_until         = now() + @lease::interval,
    request_fingerprint  = EXCLUDED.request_fingerprint,
    created_at           = now()
WHERE idempotency_keys.completed_at IS NULL
  AND idempotency_keys.leased_until < now()
RETURNING *;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE workspace_id = $1 AND user_id = $2 AND endpoint = $3 AND idempotency_key = $4;

-- name: CompleteIdempotencyKey :exec
-- Written in the same transaction as the work it describes. A response
-- recorded afterwards could fail while the work stood, and the expiring lease
-- would then let a retry do the work a second time.
UPDATE idempotency_keys
SET response_status   = $5,
    response_body     = $6,
    origin_request_id = $7,
    completed_at      = now(),
    leased_until      = NULL
WHERE workspace_id = $1 AND user_id = $2 AND endpoint = $3 AND idempotency_key = $4;

-- name: ReleaseIdempotencyKey :exec
-- The work failed, so the key is released at once rather than making a
-- legitimate retry wait out the lease. Guarded on not being complete, so a
-- late release cannot erase a finished record.
DELETE FROM idempotency_keys
WHERE workspace_id = $1 AND user_id = $2 AND endpoint = $3 AND idempotency_key = $4
  AND completed_at IS NULL;

-- name: PruneIdempotencyKeys :execrows
-- Retention. A retry is recovery from a lost response, which happens inside a
-- request-timeout window rather than days later — and these rows store a
-- response body carrying identifiers, so they are customer data and shorter is
-- better.
DELETE FROM idempotency_keys
WHERE created_at < now() - @retention::interval;
