-- +goose Up
-- +goose StatementBegin

-- The reconciler learns when a runner last changed state (Unit M5.5b).
--
-- Since M5.5b a workflow halts its runner — terminating, environment
-- destroyed — and drains its events before ending it, and the reconciler
-- leaves such a runner alone while its session is running. Without a bound
-- that skip is forever: a workflow terminated between halting and ending
-- would leave the runner terminating, and a halt whose destroy failed would
-- leave its container and checkout, with nothing ever revisiting them. The
-- time it was marked terminating is what bounds the skip.
--
-- Only the returned columns change. The return type cannot be altered in
-- place, so the function is dropped and created again, with its owner and
-- grants as 00012 set them.
DROP FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid);

CREATE FUNCTION weave_runners_to_reconcile(
    batch_size integer,
    after_created_at timestamptz,
    after_id uuid
)
RETURNS TABLE (
    id uuid, session_id uuid, workspace_id uuid, backend text,
    backend_handle text, state text, session_state text, created_at timestamptz,
    updated_at timestamptz
)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF batch_size IS NULL OR batch_size < 1 OR batch_size > 500 THEN
        RAISE EXCEPTION 'runner reconcile batch size must be between 1 and 500, got %', batch_size
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Both or neither: a half-given cursor is a caller bug, not a first page.
    IF (after_created_at IS NULL) <> (after_id IS NULL) THEN
        RAISE EXCEPTION 'runner reconcile cursor must give both created_at and id, or neither'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- Keyset pagination on (created_at, id), so the reconciler walks every
    -- live runner. The first version returned only the oldest batch: five
    -- hundred healthy old runners would have hidden every newer one — ended
    -- sessions and vanished environments never cleaned up, and a full batch
    -- also switched off the orphan sweep.
    RETURN QUERY
    SELECT r.id, r.session_id, r.workspace_id, r.backend, r.backend_handle,
           r.state, s.state, r.created_at, r.updated_at
    FROM runners r
    JOIN sessions s ON s.id = r.session_id
    WHERE r.state IN ('provisioning', 'running', 'terminating')
      AND (after_created_at IS NULL OR (r.created_at, r.id) > (after_created_at, after_id))
    ORDER BY r.created_at, r.id
    LIMIT batch_size;
END;
$$;

ALTER FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid) OWNER TO weave_rls_bypass;
GRANT SELECT ON runners TO weave_rls_bypass;
REVOKE ALL ON FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid) TO weave_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid);

CREATE FUNCTION weave_runners_to_reconcile(
    batch_size integer,
    after_created_at timestamptz,
    after_id uuid
)
RETURNS TABLE (
    id uuid, session_id uuid, workspace_id uuid, backend text,
    backend_handle text, state text, session_state text, created_at timestamptz
)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF batch_size IS NULL OR batch_size < 1 OR batch_size > 500 THEN
        RAISE EXCEPTION 'runner reconcile batch size must be between 1 and 500, got %', batch_size
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    -- Both or neither: a half-given cursor is a caller bug, not a first page.
    IF (after_created_at IS NULL) <> (after_id IS NULL) THEN
        RAISE EXCEPTION 'runner reconcile cursor must give both created_at and id, or neither'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- Keyset pagination on (created_at, id), so the reconciler walks every
    -- live runner. The first version returned only the oldest batch: five
    -- hundred healthy old runners would have hidden every newer one — ended
    -- sessions and vanished environments never cleaned up, and a full batch
    -- also switched off the orphan sweep.
    RETURN QUERY
    SELECT r.id, r.session_id, r.workspace_id, r.backend, r.backend_handle,
           r.state, s.state, r.created_at
    FROM runners r
    JOIN sessions s ON s.id = r.session_id
    WHERE r.state IN ('provisioning', 'running', 'terminating')
      AND (after_created_at IS NULL OR (r.created_at, r.id) > (after_created_at, after_id))
    ORDER BY r.created_at, r.id
    LIMIT batch_size;
END;
$$;

ALTER FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid) OWNER TO weave_rls_bypass;
GRANT SELECT ON runners TO weave_rls_bypass;
REVOKE ALL ON FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_runners_to_reconcile(integer, timestamptz, uuid) TO weave_app;

-- +goose StatementEnd
