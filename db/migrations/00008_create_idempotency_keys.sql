-- +goose Up
-- +goose StatementBegin

-- Idempotency keys: making a retried mutation safe to retry.
--
-- Row-level security is enabled here rather than in a later migration. Fourth
-- time of saying it and still true: RLS is off by default, so a tenant-owned
-- table created without it is silently unprotected and looks entirely correct.
-- See ADR-012.
--
-- The columns follow the four outcomes, decided in the tracker before this
-- file was written:
--
--   unseen              -> no row. The claim inserts one and proceeds.
--   in flight           -> a row with no response and a live lease. A second
--                          caller is refused with 409 and told to retry.
--   claimed, then died  -> the same row with an EXPIRED lease. Claimable
--                          again, which is why leased_until is a timestamp
--                          and not a boolean: a flag set by a process that
--                          never returns is a row nothing will ever clear.
--   complete            -> a row carrying the stored response, replayed.
--
-- A fifth case the spec did not name and the tracker does: the work failed.
-- The row is deleted immediately so a legitimate retry is not made to wait
-- out the lease. If the process dies before deleting, the lease covers it.
CREATE TABLE idempotency_keys (
    id            uuid        PRIMARY KEY,
    workspace_id  uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- The scope. A key is a caller's own string, so two callers may pick the
    -- same one; and the same string against two endpoints is two unrelated
    -- requests. Scoping by neither would make one caller's retry collide with
    -- another's first attempt.
    user_id       uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    endpoint      text        NOT NULL,
    idempotency_key text      NOT NULL,

    -- The fencing token, reissued on every claim including a reclaim.
    --
    -- Without it a lease is unsafe rather than merely optimistic. A holder that
    -- is slow rather than dead comes back after its lease expired and a second
    -- caller has reclaimed the key — and completion and release, matching only
    -- on scope and key, would let it write the second caller's record or
    -- delete their claim while both are creating sessions. The token is what
    -- makes a late writer's update match nothing instead.
    claimant      uuid        NOT NULL,

    -- The canonical fingerprint of the request body, not the body itself.
    -- Comparing raw bytes would reject retries that are equivalent — a client
    -- that reorders JSON members or sends `base_branch` empty where it omitted
    -- it before has not changed its request.
    request_fingerprint bytea NOT NULL,

    -- The stored response, absent while the work is in flight. Status and body
    -- bytes rather than a re-encoding, so the second answer cannot differ from
    -- the first by a field order or a formatting change.
    response_status integer,
    response_body   bytea,
    -- The request id of the attempt that did the work. Not replayed to the
    -- caller — the response carries the retry's own id, so it matches the log
    -- line for the request actually made — but recorded so a replay can name
    -- the original in its log line and an operator can get from one to the
    -- other.
    origin_request_id text,

    -- The lease. NULL once complete; a time in the past means the holder died.
    leased_until  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    completed_at  timestamptz,

    -- The exclusivity that makes exactly one claim win. Without it the
    -- implementation is a check followed by an insert, and two concurrent
    -- requests both check, both find nothing, and both create a session —
    -- which is the entire failure this table exists to prevent, reintroduced
    -- by the storage layer.
    CONSTRAINT idempotency_keys_scope_key
        UNIQUE (workspace_id, user_id, endpoint, idempotency_key),

    CONSTRAINT idempotency_keys_key_not_blank
        CHECK (length(btrim(idempotency_key)) > 0),
    -- Bounded because it is caller-supplied. 255 is generous for a UUID or a
    -- hash and small enough that the unique index stays sane.
    CONSTRAINT idempotency_keys_key_length
        CHECK (length(idempotency_key) <= 255),
    CONSTRAINT idempotency_keys_endpoint_not_blank
        CHECK (length(btrim(endpoint)) > 0),
    CONSTRAINT idempotency_keys_endpoint_length
        CHECK (length(endpoint) <= 100),
    CONSTRAINT idempotency_keys_fingerprint_length
        CHECK (length(request_fingerprint) = 32),
    -- Bounded so a large response cannot make the table the place big blobs
    -- live. A session response is a few hundred bytes; anything approaching
    -- this is a sign the endpoint should not be replaying bodies at all.
    CONSTRAINT idempotency_keys_response_body_length
        CHECK (response_body IS NULL OR length(response_body) <= 65536),
    -- Complete means all three together. A row with a status and no body, or a
    -- completion time and no status, is a half-written record that a replay
    -- would turn into a malformed answer.
    CONSTRAINT idempotency_keys_complete_together
        CHECK (
            (completed_at IS NULL AND response_status IS NULL AND response_body IS NULL)
            OR
            (completed_at IS NOT NULL AND response_status IS NOT NULL AND response_body IS NOT NULL)
        ),
    CONSTRAINT idempotency_keys_response_status_valid
        CHECK (response_status IS NULL OR (response_status >= 100 AND response_status < 600))
);

-- Retention: rows are pruned on age, so the sweep reads this.
CREATE INDEX idempotency_keys_created_idx ON idempotency_keys (created_at);
CREATE INDEX idempotency_keys_workspace_idx ON idempotency_keys (workspace_id);

-- --------------------------------------------------------------------------
-- Grants and policies
--
-- Same shape as the migrations before: the application connects as weave_app,
-- which is what carries isolation; FORCE is the second layer; an absent
-- context matches no row.
--
-- DELETE is granted, unlike sessions: releasing a key whose work failed is a
-- delete, and it is the application's job rather than an operator's.
-- --------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE, DELETE ON idempotency_keys TO weave_app;

ALTER TABLE idempotency_keys ENABLE ROW LEVEL SECURITY;
ALTER TABLE idempotency_keys FORCE  ROW LEVEL SECURITY;

CREATE POLICY idempotency_keys_tenant_read ON idempotency_keys FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY idempotency_keys_tenant_insert ON idempotency_keys FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY idempotency_keys_tenant_update ON idempotency_keys FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY idempotency_keys_tenant_delete ON idempotency_keys FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

-- --------------------------------------------------------------------------
-- Retention
--
-- The sweep runs from a background goroutine with no tenant context, and under
-- FORCE row-level security every policy then matches nothing — so a plain
-- DELETE removes no rows and reports success. Retention would silently never
-- happen, on a table holding response bodies that carry identifiers, and
-- nothing would say so.
--
-- The same shape ADR-012 settled on for the invitation lookup: a SECURITY
-- DEFINER function owned by weave_rls_bypass, scoped so narrowly that holding
-- it grants nothing else. It deletes by age and returns a count; it cannot
-- read a row, name a workspace, or remove anything still within the window.
-- --------------------------------------------------------------------------

CREATE FUNCTION weave_prune_idempotency_keys(retention interval)
RETURNS bigint
LANGUAGE plpgsql VOLATILE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    removed bigint;
BEGIN
    DELETE FROM idempotency_keys WHERE created_at < now() - retention;
    GET DIAGNOSTICS removed = ROW_COUNT;
    RETURN removed;
END;
$$;

ALTER FUNCTION weave_prune_idempotency_keys(interval) OWNER TO weave_rls_bypass;

-- DELETE, and SELECT on exactly the one column the WHERE clause reads: a
-- delete has to read before it removes, so DELETE alone fails with "permission
-- denied" — measured. Column-level rather than table-level, so the role that
-- looks past the policies can still not read a key, a fingerprint or a stored
-- response body.
GRANT DELETE ON idempotency_keys TO weave_rls_bypass;
GRANT SELECT (created_at) ON idempotency_keys TO weave_rls_bypass;

-- Revoked from PUBLIC before being granted, so the function is reachable only
-- by the role that needs it rather than by anything that can connect.
REVOKE EXECUTE ON FUNCTION weave_prune_idempotency_keys(interval) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_prune_idempotency_keys(interval) TO weave_app;

COMMENT ON TABLE idempotency_keys IS 'Makes a retried mutation safe. The claim commits before the work and carries a lease, because a claim inside the work''s transaction cannot refuse a concurrent caller.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS idempotency_keys_tenant_delete ON idempotency_keys;
DROP POLICY IF EXISTS idempotency_keys_tenant_update ON idempotency_keys;
DROP POLICY IF EXISTS idempotency_keys_tenant_insert ON idempotency_keys;
DROP POLICY IF EXISTS idempotency_keys_tenant_read ON idempotency_keys;

DROP FUNCTION IF EXISTS weave_prune_idempotency_keys(interval);
DROP TABLE IF EXISTS idempotency_keys;

-- +goose StatementEnd
