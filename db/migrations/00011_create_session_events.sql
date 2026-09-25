-- +goose Up
-- +goose StatementBegin

-- Session events, their sequence, and the quarantine (Unit M5.3).
--
-- The first table filled from the execution plane. What a runner sends is
-- provider output and may carry customer source, and the runner itself is
-- untrusted — so the table is append-only, tenant-scoped under FORCE RLS from
-- this migration, and written only after the ingestor has checked every
-- identifier the event claims against rows it did not supply.

-- --------------------------------------------------------------------------
-- The per-session counter
--
-- Separate from `sessions` so ingesting an event does not lock the row every
-- state transition locks: a busy session emitting output must not queue
-- behind, or ahead of, its own workflow.
--
-- The ingestor increments it and inserts the event in one transaction. A
-- duplicate is detected by the insert's unique constraint, and the
-- transaction is rolled back — which takes the increment with it. That is how
-- a duplicate consumes no sequence: not by checking first, which two replicas
-- can both pass, but by never committing the number at all.
-- --------------------------------------------------------------------------
CREATE TABLE session_event_sequences (
    session_id    uuid    PRIMARY KEY,
    workspace_id  uuid    NOT NULL,
    last_sequence bigint  NOT NULL DEFAULT 0,

    CONSTRAINT session_event_sequences_session_fkey
        FOREIGN KEY (session_id, workspace_id)
        REFERENCES sessions (id, workspace_id) ON DELETE CASCADE,
    CONSTRAINT session_event_sequences_not_negative CHECK (last_sequence >= 0)
);

-- --------------------------------------------------------------------------
-- The events
-- --------------------------------------------------------------------------
CREATE TABLE session_events (
    session_id     uuid        NOT NULL,
    -- Gapless from 1, per session: the order of acceptance, which is the only
    -- order the control plane can vouch for. `occurred_at` is a claim.
    sequence       bigint      NOT NULL,
    -- From the session row, never from the event. The envelope's own
    -- workspace_id is checked against this and a mismatch is quarantined.
    workspace_id   uuid        NOT NULL,
    -- The producing runner. Not yet bound to the session — that needs a
    -- runner record, which is M5.4.
    runner_id      uuid        NOT NULL,
    event_id       uuid        NOT NULL,
    type           text        NOT NULL,
    schema_version text        NOT NULL,
    correlation_id uuid        NOT NULL,
    occurred_at    timestamptz NOT NULL,
    received_at    timestamptz NOT NULL,
    -- Re-encoded from the validated type, so a field the schema does not know
    -- is never stored.
    payload        jsonb       NOT NULL,

    PRIMARY KEY (session_id, sequence),
    -- The deduplication key the architecture states. Enforced here rather
    -- than by a read first: M4.2 and M5.0 both shipped a check-then-insert
    -- that passed its concurrency test until the test really overlapped.
    CONSTRAINT session_events_dedup_key UNIQUE (session_id, runner_id, event_id),
    CONSTRAINT session_events_session_fkey
        FOREIGN KEY (session_id, workspace_id)
        REFERENCES sessions (id, workspace_id) ON DELETE CASCADE,
    CONSTRAINT session_events_sequence_positive CHECK (sequence > 0),
    CONSTRAINT session_events_type_known CHECK (type IN ('message.created', 'provider.failed')),
    CONSTRAINT session_events_schema_version_format CHECK (schema_version ~ '^1\.[0-9]+$'),
    CONSTRAINT session_events_payload_is_object CHECK (jsonb_typeof(payload) = 'object'),
    -- Belt and braces behind the ingestor's own limit: "payloads stay small".
    CONSTRAINT session_events_payload_size CHECK (pg_column_size(payload) <= 65536)
);

CREATE INDEX session_events_workspace_idx ON session_events (workspace_id);

-- Append-only, as session_state_transitions is: a row trigger for UPDATE and
-- DELETE, a statement trigger for TRUNCATE, which fires no row trigger at all.
-- A cascade from deleting the session itself is permitted, for the reason
-- 00007 records — refusing it would make a tenant undeletable.
CREATE FUNCTION session_events_reject_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1
       AND NOT EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) THEN
        RETURN OLD;
    END IF;

    RAISE EXCEPTION
        'session_events is append-only: invariant 9 makes session history append-only and ordered, so a correction is a new event rather than an edit to an old one.'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER session_events_append_only
    BEFORE UPDATE OR DELETE ON session_events
    FOR EACH ROW EXECUTE FUNCTION session_events_reject_mutation();

CREATE TRIGGER session_events_no_truncate
    BEFORE TRUNCATE ON session_events
    FOR EACH STATEMENT EXECUTE FUNCTION session_events_reject_mutation();

-- --------------------------------------------------------------------------
-- The quarantine
--
-- What the ingestor refused, with what an operator needs and nothing a tenant
-- would not want kept: **no payload**, per context/code-standards.md. A hash of
-- it correlates repeats without retaining the content.
--
-- **Not tenant-owned, and not readable by the application at all.** Many
-- rows have no workspace — an event for an unknown session, or one whose
-- subject would not parse, never resolved one — so there is no tenant for a
-- policy to scope by. Rather than a policy with a NULL-workspace hole in it,
-- `weave_app` may INSERT and nothing else; operators read it as the owner.
-- `workspace_id`, when present, is the one resolved from the session row,
-- never the one the event claimed.
-- --------------------------------------------------------------------------
CREATE TABLE session_event_quarantine (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    received_at    timestamptz NOT NULL DEFAULT now(),
    subject        text        NOT NULL,
    reason         text        NOT NULL,
    event_id       uuid,
    session_id     uuid,
    workspace_id   uuid,
    schema_version text,
    size_bytes     integer     NOT NULL,
    delivery_count integer     NOT NULL,
    payload_sha256 text        NOT NULL,

    CONSTRAINT session_event_quarantine_reason_known CHECK (reason IN (
        'malformed', 'oversize', 'unknown_major_version', 'unknown_type',
        'invalid_payload', 'subject_mismatch', 'workspace_mismatch',
        'unknown_session', 'session_terminal', 'delivery_exhausted',
        'unparseable_subject'
    )),
    CONSTRAINT session_event_quarantine_subject_length CHECK (length(subject) <= 256),
    CONSTRAINT session_event_quarantine_version_length CHECK (schema_version IS NULL OR length(schema_version) <= 32),
    CONSTRAINT session_event_quarantine_sha_format CHECK (payload_sha256 ~ '^[0-9a-f]{64}$'),
    CONSTRAINT session_event_quarantine_counts CHECK (size_bytes >= 0 AND delivery_count >= 1)
);

CREATE INDEX session_event_quarantine_received_idx ON session_event_quarantine (received_at);

-- --------------------------------------------------------------------------
-- Resolving a session for an event
--
-- The ingestor consumes every session's subject, so it has no tenant context,
-- and under FORCE RLS a direct read of `sessions` matches no row while
-- reporting success — the wall M5.0's sweep and M5.1's publisher both hit.
--
-- So this one lookup is privileged, and it is bounded the way M5.0 and M5.1
-- learned to bound them: one session id in, at most one row out, NULL refused.
-- It cannot list, filter or range, so it cannot enumerate sessions — a caller
-- learns about a session only by already holding its id, which is exactly
-- what the subject gives the ingestor. Everything after it runs in an
-- ordinary tenant transaction scoped by the workspace **this** returns.
-- --------------------------------------------------------------------------
CREATE FUNCTION weave_session_for_event(presented_session_id uuid)
RETURNS TABLE (workspace_id uuid, state text)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF presented_session_id IS NULL THEN
        RAISE EXCEPTION 'a session id is required'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;

    RETURN QUERY
    SELECT s.workspace_id, s.state
    FROM sessions s
    WHERE s.id = presented_session_id;
END;
$$;

ALTER FUNCTION weave_session_for_event(uuid) OWNER TO weave_rls_bypass;
GRANT SELECT ON sessions TO weave_rls_bypass;
REVOKE ALL ON FUNCTION weave_session_for_event(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_session_for_event(uuid) TO weave_app;

-- --------------------------------------------------------------------------
-- Grants and policies
-- --------------------------------------------------------------------------

-- Append-only in grants as well as triggers: no UPDATE or DELETE to hold.
GRANT SELECT, INSERT ON session_events TO weave_app;
GRANT SELECT, INSERT, UPDATE ON session_event_sequences TO weave_app;
-- INSERT only. See the quarantine's comment above.
GRANT INSERT ON session_event_quarantine TO weave_app;

ALTER TABLE session_events          ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_events          FORCE  ROW LEVEL SECURITY;
ALTER TABLE session_event_sequences ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_event_sequences FORCE  ROW LEVEL SECURITY;

CREATE POLICY session_events_tenant_read ON session_events FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY session_events_tenant_insert ON session_events FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY session_event_sequences_tenant_read ON session_event_sequences FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY session_event_sequences_tenant_insert ON session_event_sequences FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY session_event_sequences_tenant_update ON session_event_sequences FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());

COMMENT ON TABLE session_events IS 'Append-only session history from the execution plane. Ordered by a gapless per-session sequence assigned at ingestion; deduplicated by (session_id, runner_id, event_id).';
COMMENT ON TABLE session_event_quarantine IS 'Events the ingestor refused. No payload, by rule; operator-only, since many rows resolve to no workspace.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP FUNCTION IF EXISTS weave_session_for_event(uuid);
REVOKE SELECT ON sessions FROM weave_rls_bypass;
DROP TABLE IF EXISTS session_event_quarantine;
DROP TRIGGER IF EXISTS session_events_no_truncate ON session_events;
DROP TRIGGER IF EXISTS session_events_append_only ON session_events;
DROP TABLE IF EXISTS session_events;
DROP FUNCTION IF EXISTS session_events_reject_mutation();
DROP TABLE IF EXISTS session_event_sequences;

-- +goose StatementEnd
