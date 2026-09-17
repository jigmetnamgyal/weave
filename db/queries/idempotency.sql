-- name: ClaimIdempotencyKey :one
-- The claim, as one statement rather than a check followed by an insert.
--
-- ON CONFLICT DO UPDATE rather than DO NOTHING, because the conflict has two
-- meanings and only the update can tell them apart while taking the lease: a
-- row whose lease has expired and which never completed is a claim whose
-- holder died, and it is claimable again; anything else is either in flight or
-- complete and must not be disturbed.
--
-- The WHERE on the update is what makes that safe. It also requires the
-- fingerprint to match, so a reclaim with a *different* request does not
-- quietly overwrite what the first attempt asked for — it falls through to the
-- read below and is reported as the mismatch it is.
--
-- `claimant` is reissued on every claim. It is the fencing token: a holder
-- that was slow rather than dead must not be able to complete or release the
-- claim that replaced it.
INSERT INTO idempotency_keys (
    id, workspace_id, user_id, endpoint, idempotency_key,
    claimant, request_fingerprint, leased_until
)
VALUES ($1, $2, $3, $4, $5, @claimant, $6, now() + @lease::interval)
ON CONFLICT (workspace_id, user_id, endpoint, idempotency_key) DO UPDATE
SET claimant     = EXCLUDED.claimant,
    leased_until = now() + @lease::interval,
    created_at   = now()
WHERE idempotency_keys.completed_at IS NULL
  AND idempotency_keys.leased_until < now()
  AND idempotency_keys.request_fingerprint = EXCLUDED.request_fingerprint
RETURNING *;

-- name: GetIdempotencyKey :one
SELECT * FROM idempotency_keys
WHERE workspace_id = $1 AND user_id = $2 AND endpoint = $3 AND idempotency_key = $4;

-- name: CompleteIdempotencyKey :execrows
-- Written in the same transaction as the work it describes. A response
-- recorded afterwards could fail while the work stood, and the expiring lease
-- would then let a retry do the work a second time.
--
-- Fenced on `claimant`: a holder whose lease expired while it was still
-- running must not write over the record belonging to whoever reclaimed the
-- key. Returning the row count is what lets the caller notice that happened.
UPDATE idempotency_keys
SET response_status   = $6,
    response_body     = $7,
    origin_request_id = $8,
    completed_at      = now(),
    leased_until      = NULL
WHERE workspace_id = $1 AND user_id = $2 AND endpoint = $3 AND idempotency_key = $4
  AND claimant = $5
  AND completed_at IS NULL;

-- name: ReleaseIdempotencyKey :execrows
-- The work failed, so the key is released at once rather than making a
-- legitimate retry wait out the lease. Fenced for the same reason completion
-- is: a late holder must not delete the claim that replaced it. Guarded on not
-- being complete, so a late release cannot erase a finished record.
DELETE FROM idempotency_keys
WHERE workspace_id = $1 AND user_id = $2 AND endpoint = $3 AND idempotency_key = $4
  AND claimant = $5
  AND completed_at IS NULL;

-- name: PruneIdempotencyKeys :one
-- Retention, through a SECURITY DEFINER function.
--
-- The sweep runs from a background goroutine with no tenant context, and under
-- FORCE row-level security every policy then matches nothing — so a direct
-- DELETE removes no rows and reports success, which is retention silently not
-- happening. The same problem invitations-by-token had, and the same answer
-- ADR-012 settled on: a function owned by a role that may look past the
-- policies, scoped so narrowly that it can do nothing else.
SELECT weave_prune_idempotency_keys(@retention::interval);
