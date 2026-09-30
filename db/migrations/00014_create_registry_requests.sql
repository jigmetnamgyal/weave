-- +goose Up
-- +goose StatementBegin

-- Registry requests (Unit M5.4c, ADR-016).
--
-- Every package-registry request a runner makes is recorded here, per session,
-- **before** the registry proxy forwards it: ADR-013's control on the registry
-- side channel, so a session fetching packages its repository names nowhere is
-- visible. Append-only — a record of what was fetched is worth nothing if it
-- can be edited — and tenant-owned, with row-level security in this migration.
--
-- What is kept is deliberately little: method, host and **path**. Never the
-- query string (it can carry a token), never a header, never a body. The path
-- is the package.
CREATE TABLE registry_requests (
    id           uuid        PRIMARY KEY,
    workspace_id uuid        NOT NULL,
    session_id   uuid        NOT NULL,
    runner_id    uuid        NOT NULL,
    host         text        NOT NULL,
    method       text        NOT NULL,
    path         text        NOT NULL,
    requested_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT registry_requests_session_fkey
        FOREIGN KEY (session_id, workspace_id)
        REFERENCES sessions (id, workspace_id) ON DELETE CASCADE,
    CONSTRAINT registry_requests_runner_fkey
        FOREIGN KEY (runner_id) REFERENCES runners (id) ON DELETE CASCADE,
    -- An exact lowercase hostname, as the runner manager put on the policy.
    CONSTRAINT registry_requests_host_valid
        CHECK (host ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$' AND length(host) <= 253),
    CONSTRAINT registry_requests_method_known
        CHECK (method IN ('GET', 'HEAD', 'POST', 'PUT', 'PATCH', 'DELETE', 'OPTIONS')),
    -- Absolute, bounded, and without a query: the proxy strips it and caps the
    -- rest; the table refuses anything that got past that.
    CONSTRAINT registry_requests_path_shape
        CHECK (path LIKE '/%' AND position('?' IN path) = 0 AND length(path) <= 2048)
);

CREATE INDEX registry_requests_session_idx ON registry_requests (session_id, requested_at);
CREATE INDEX registry_requests_workspace_idx ON registry_requests (workspace_id);

-- Append-only, as session_events: no update, delete or truncate, except the
-- cascade when the session (or its workspace) is deleted — the record then has
-- nothing left to be about.
CREATE FUNCTION registry_requests_reject_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1
       AND NOT EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION
        'registry_requests is append-only: a record of what a session fetched is worth nothing if it can be edited (ADR-016).'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER registry_requests_append_only
    BEFORE UPDATE OR DELETE ON registry_requests
    FOR EACH ROW EXECUTE FUNCTION registry_requests_reject_mutation();
CREATE TRIGGER registry_requests_no_truncate
    BEFORE TRUNCATE ON registry_requests
    FOR EACH STATEMENT EXECUTE FUNCTION registry_requests_reject_mutation();

ALTER TABLE registry_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE registry_requests FORCE  ROW LEVEL SECURITY;

CREATE POLICY registry_requests_tenant_read ON registry_requests FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY registry_requests_tenant_insert ON registry_requests FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

GRANT SELECT, INSERT ON registry_requests TO weave_app;

COMMENT ON TABLE registry_requests IS
    'Every package-registry request a runner made, recorded before the registry proxy forwarded it (ADR-016). Append-only.';

-- --------------------------------------------------------------------------
-- The registry proxy's one privileged read.
--
-- A forwarded request names its runner (through the sandbox name Vercel signs)
-- and nothing else. The proxy has no tenant context, and under FORCE RLS a
-- direct read of runners matches nothing. So, as the ingestor resolves a
-- session: one id in, at most one row out — the runner's session, workspace,
-- backend and state — so it cannot enumerate. Every write after it runs in the
-- tenant context of the workspace it returns.
-- --------------------------------------------------------------------------
CREATE FUNCTION weave_runner_for_registry_request(presented_runner_id uuid)
RETURNS TABLE (session_id uuid, workspace_id uuid, backend text, state text)
LANGUAGE plpgsql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF presented_runner_id IS NULL THEN
        RAISE EXCEPTION 'a runner id is required'
            USING ERRCODE = 'invalid_parameter_value';
    END IF;
    RETURN QUERY
    SELECT r.session_id, r.workspace_id, r.backend, r.state
    FROM runners r
    WHERE r.id = presented_runner_id;
END;
$$;

ALTER FUNCTION weave_runner_for_registry_request(uuid) OWNER TO weave_rls_bypass;
GRANT SELECT ON runners TO weave_rls_bypass;
REVOKE ALL ON FUNCTION weave_runner_for_registry_request(uuid) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_runner_for_registry_request(uuid) TO weave_app;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION weave_runner_for_registry_request(uuid);
DROP TABLE registry_requests;
DROP FUNCTION registry_requests_reject_mutation();
-- +goose StatementEnd
