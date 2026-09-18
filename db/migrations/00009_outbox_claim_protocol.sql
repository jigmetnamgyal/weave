-- +goose Up
-- +goose StatementBegin

-- The outbox claim protocol, completed.
--
-- M4.2 designed the lease and left it unexercised because nothing claimed a
-- row. Building the claimer surfaced two gaps that a design nobody had run
-- could not have shown:
--
-- 1. **No fencing token.** `leased_until` alone tells a writer nothing about
--    whether the claim it is finishing is still its own. A publisher that
--    paused past its lease returns after another has reclaimed the row and
--    completes, retries or terminates *that* claim. This is the identical
--    hole M5.0's review found in `idempotency_keys`, in the sibling table,
--    and the first draft of the M5.1 spec quoted that lesson without noticing
--    it applied here.
--
-- 2. **Nowhere to put "stop trying".** The claim predicate is
--    `completed_at IS NULL`, so a row that can never succeed had two
--    available representations and both are wrong: marked complete, which
--    lies to anyone counting deliveries, or left pending, which retries
--    forever.

-- The fence. Null until first claimed; reissued on every claim, including a
-- reclaim after an expired lease — which is precisely the case it exists for.
ALTER TABLE outbox_events ADD COLUMN claimant uuid;

-- Terminal, as its own column rather than a value of completed_at.
--
-- Separate because the two mean different things to anyone reading the table:
-- completed is work that happened, terminated is work that never will. An
-- operator replaying one deliberately clears this and the row becomes
-- claimable again, which is a decision someone makes rather than a state the
-- system drifts into.
ALTER TABLE outbox_events ADD COLUMN terminated_at timestamptz;

-- `last_error` already exists and is deliberately kept on a terminated row:
-- context/code-standards.md requires a quarantine path that retains failure
-- metadata, and a dead row with no reason is one nobody can act on.

ALTER TABLE outbox_events
    ADD CONSTRAINT outbox_events_not_both_outcomes
    CHECK (completed_at IS NULL OR terminated_at IS NULL);

-- The claim index, replaced so a terminated row is excluded the way a
-- completed one is. A partial index that still matched dead rows would make
-- every poll walk past them forever.
DROP INDEX IF EXISTS outbox_events_claimable_idx;
CREATE INDEX outbox_events_claimable_idx
    ON outbox_events (available_at)
    WHERE completed_at IS NULL AND terminated_at IS NULL;

-- --------------------------------------------------------------------------
-- Claiming across workspaces
--
-- The publisher polls every tenant's queue, so it is the one caller that
-- cannot have a workspace context — and under FORCE row-level security an
-- absent context matches no row. A direct poll would read nothing and report
-- success, which is exactly how M5.0's retention sweep silently never ran.
--
-- So the claim is privileged, and nothing else is. The rows it returns carry
-- their own `workspace_id`, and every write after the claim runs in an
-- ordinary tenant transaction scoped by **that** value — taken from the row,
-- never from the caller. A privileged path that accepted a workspace id from
-- its caller would be an authorization bypass through a user-controlled key,
-- and being callable only by the publisher is not the protection, for the
-- same reason it was not on M5.0's prune function.
--
-- Both arguments are bounded here rather than trusted. M5.0 shipped a
-- privileged helper that was narrow in shape and wide open in range — it took
-- any interval, and `interval '0'` would have emptied a table for every
-- tenant. The lesson is that a privileged function validates its inputs even
-- when only one caller exists today.
-- --------------------------------------------------------------------------

CREATE FUNCTION weave_claim_outbox_batch(batch_size integer, lease interval, max_attempts integer)
RETURNS SETOF outbox_events
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF batch_size IS NULL OR batch_size < 1 OR batch_size > 100 THEN
        RAISE EXCEPTION 'outbox batch size must be between 1 and 100, got %', batch_size
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- A lease shorter than the work is a lease that expires mid-delivery and
    -- invites the double-publish the fence then has to catch; one longer than
    -- a few minutes strands a row when a publisher dies.
    IF lease IS NULL OR lease < interval '10 seconds' OR lease > interval '10 minutes' THEN
        RAISE EXCEPTION 'outbox lease must be between 10 seconds and 10 minutes, got %', lease
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Bounded like the others. A ceiling of zero would terminate every row on
    -- the next poll; an enormous one would let a poison row outlive the
    -- workflow deduplication it is meant to stay inside.
    IF max_attempts IS NULL OR max_attempts < 1 OR max_attempts > 100 THEN
        RAISE EXCEPTION 'outbox max attempts must be between 1 and 100, got %', max_attempts
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- The ceiling, enforced here rather than only by the publisher.
    --
    -- `Publisher.settleRetry` gives up after enough attempts, but it only runs
    -- when a publisher reaches settlement. A publisher that dies mid-delivery
    -- never does: its lease expires, the row is claimable again, and that can
    -- repeat forever — so the ceiling that bounds the duplicate guarantee
    -- would never apply on the one path most likely to need it.
    --
    -- Terminated rather than skipped, so an exhausted row leaves the queue
    -- with its reason instead of being walked past on every poll.
    -- `left(..., 2000)` is load-bearing, not tidiness.
    --
    -- `last_error` is capped at 2000 characters, and a retry can leave it
    -- exactly there. Appending the termination reason then violates the CHECK
    -- — which aborts this whole function, so **one poison row stops every
    -- other row in every workspace from being claimed at all**. Measured: a
    -- row with a 2000-character error made the claim raise rather than return
    -- a batch.
    --
    -- The reason goes first because it is the part worth keeping: what is
    -- truncated is the older failure, which is already in the logs.
    UPDATE outbox_events
    SET terminated_at = now(),
        leased_until  = NULL,
        last_error    = left(
            'gave up after ' || attempts || ' attempts without reaching settlement' ||
            CASE WHEN last_error IS NULL THEN '' ELSE ' | ' || last_error END,
            2000)
    WHERE completed_at IS NULL
      AND terminated_at IS NULL
      AND attempts >= max_attempts
      AND available_at <= now()
      AND (leased_until IS NULL OR leased_until < now());

    RETURN QUERY
    WITH due AS (
        SELECT id FROM outbox_events
        WHERE completed_at IS NULL
          AND terminated_at IS NULL
          AND available_at <= now()
          AND (leased_until IS NULL OR leased_until < now())
        ORDER BY available_at, id
        LIMIT batch_size
        FOR UPDATE SKIP LOCKED
    )
    UPDATE outbox_events
    SET claimant     = gen_random_uuid(),
        leased_until = now() + lease,
        attempts     = attempts + 1
    FROM due
    WHERE outbox_events.id = due.id
    RETURNING outbox_events.*;
END;
$$;

ALTER FUNCTION weave_claim_outbox_batch(integer, interval, integer) OWNER TO weave_rls_bypass;
GRANT SELECT, UPDATE ON outbox_events TO weave_rls_bypass;

REVOKE EXECUTE ON FUNCTION weave_claim_outbox_batch(integer, interval, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_claim_outbox_batch(integer, interval, integer) TO weave_app;

COMMENT ON COLUMN outbox_events.claimant IS 'Fencing token, reissued on every claim. A publisher whose lease expired must not be able to complete, retry or terminate the claim that replaced it.';
COMMENT ON COLUMN outbox_events.terminated_at IS 'Set when a row will never succeed. Distinct from completed_at, which would claim work that never happened.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS outbox_events_claimable_idx;
CREATE INDEX outbox_events_claimable_idx
    ON outbox_events (available_at)
    WHERE completed_at IS NULL;

DROP FUNCTION IF EXISTS weave_claim_outbox_batch(integer, interval, integer);

ALTER TABLE outbox_events DROP CONSTRAINT IF EXISTS outbox_events_not_both_outcomes;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS terminated_at;
ALTER TABLE outbox_events DROP COLUMN IF EXISTS claimant;

-- +goose StatementEnd
