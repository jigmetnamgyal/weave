-- name: ClaimOutboxBatch :many
-- Take a batch of due rows, across every workspace, through the privileged
-- function.
--
-- Privileged because the publisher is the one caller that cannot have a
-- workspace context — it polls every tenant's queue — and under FORCE
-- row-level security an absent context matches no row, so a direct poll reads
-- nothing and reports success. The function bounds its own arguments and
-- returns rows carrying their `workspace_id`, which is what every write after
-- the claim is scoped by: taken from the row, never from the caller.
SELECT * FROM weave_claim_outbox_batch(@batch_size::int, @lease::interval, @max_attempts::int);

-- name: CompleteOutboxEvent :execrows
-- Fenced. A stale publisher marking someone else's claim done would hide work
-- that is still owed.
UPDATE outbox_events
SET completed_at = now(),
    leased_until = NULL,
    last_error   = NULL
WHERE id = $1 AND claimant = $2
  AND completed_at IS NULL AND terminated_at IS NULL;

-- name: RetryOutboxEvent :execrows
-- The delivery failed for a reason that might not recur: release the lease,
-- record why, and push the row out by the caller's backoff.
--
-- Fenced for the same reason completion is — a stale publisher pushing out
-- someone else's row would delay work that is currently being done.
UPDATE outbox_events
SET leased_until = NULL,
    available_at = now() + @backoff::interval,
    last_error   = @last_error
WHERE id = $1 AND claimant = $2
  AND completed_at IS NULL AND terminated_at IS NULL;

-- name: TerminateOutboxEvent :execrows
-- The delivery will never succeed, or has failed too many times to keep
-- trying. The row stops being claimed and keeps its reason.
--
-- Fenced like the other two. The first version of the M5.1 spec fenced
-- "completion and retry" and left this one out, which would have let a stale
-- publisher quarantine the row that replaced it.
UPDATE outbox_events
SET terminated_at = now(),
    leased_until  = NULL,
    last_error    = @last_error
WHERE id = $1 AND claimant = $2
  AND completed_at IS NULL AND terminated_at IS NULL;

-- name: GetOutboxEvent :one
-- Used by tests and by an operator looking at one row. Workspace-scoped like
-- everything else, so a row id from one tenant is not readable through
-- another.
SELECT * FROM outbox_events WHERE id = $1 AND workspace_id = $2;

-- name: ReleaseUnstartedOutboxEvent :execrows
-- Give a row back that was claimed and never attempted.
--
-- The claim increments `attempts` for the whole batch, which is deliberate: a
-- publisher that dies mid-delivery must still burn one, or the ceiling that
-- bounds the duplicate guarantee never applies to the case it exists for. But
-- a row the publisher never got to — because the batch ran out of lease — has
-- not been attempted at all, and spending an attempt on it would march it
-- toward termination for no reason but our own slowness.
--
-- So the attempt is given back, and `available_at` is left alone so the row is
-- claimable immediately rather than pushed out like a failure. Fenced like
-- every other settling update.
UPDATE outbox_events
SET leased_until = NULL,
    claimant     = NULL,
    attempts     = GREATEST(attempts - 1, 0)
WHERE id = $1 AND claimant = $2
  AND completed_at IS NULL AND terminated_at IS NULL;
