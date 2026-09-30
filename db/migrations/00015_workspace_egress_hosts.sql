-- +goose Up
-- +goose StatementBegin

-- Canonical persisted shape. DNS is intentionally not consulted here.
CREATE FUNCTION weave_egress_hostname_valid(host text) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT
SET search_path = public, pg_temp
AS $$
    SELECT length(host) BETWEEN 3 AND 253
      AND host ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$'
      AND split_part(host, '.', -1) ~ '[a-z]'
      AND host !~ '(^|\.)(localhost|local|localdomain|internal|home\.arpa|onion)$'
      AND NOT EXISTS (
          SELECT 1 FROM unnest(string_to_array(host, '.')) AS label
          WHERE length(label) > 63 OR label LIKE 'xn--%'
      );
$$;

CREATE TABLE workspace_egress_hosts (
    id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    hostname text NOT NULL CHECK (weave_egress_hostname_valid(hostname)),
    created_by uuid NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT workspace_egress_hosts_workspace_host_key UNIQUE (workspace_id, hostname)
);
ALTER TABLE workspace_egress_hosts ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_egress_hosts FORCE ROW LEVEL SECURITY;
CREATE POLICY workspace_egress_hosts_tenant_read ON workspace_egress_hosts FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY workspace_egress_hosts_tenant_insert ON workspace_egress_hosts FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY workspace_egress_hosts_tenant_delete ON workspace_egress_hosts FOR DELETE
    USING (workspace_id = weave_current_workspace_id());
GRANT SELECT, INSERT, DELETE ON workspace_egress_hosts TO weave_app;

-- Cap is serialized even for a direct INSERT, not only for the API's adapter.
CREATE FUNCTION weave_check_egress_host_cap() RETURNS trigger
LANGUAGE plpgsql SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.workspace_id IS DISTINCT FROM weave_current_workspace_id() THEN
        RAISE EXCEPTION 'egress host tenant mismatch' USING ERRCODE = 'insufficient_privilege';
    END IF;
    PERFORM id FROM workspaces WHERE id = NEW.workspace_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'workspace not found' USING ERRCODE = 'foreign_key_violation';
    END IF;
    IF (SELECT count(*) FROM workspace_egress_hosts WHERE workspace_id = NEW.workspace_id) >= 20 THEN
        RAISE EXCEPTION 'egress host limit reached'
          USING ERRCODE = 'check_violation', CONSTRAINT = 'workspace_egress_hosts_cap';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER workspace_egress_hosts_cap BEFORE INSERT ON workspace_egress_hosts
    FOR EACH ROW EXECUTE FUNCTION weave_check_egress_host_cap();

-- A single explicit row distinguishes an empty snapshot from missing data.
ALTER TABLE runners ADD CONSTRAINT runners_id_session_workspace_key UNIQUE (id, session_id, workspace_id);
CREATE FUNCTION weave_egress_snapshot_hosts_valid(hosts text[]) RETURNS boolean
LANGUAGE sql IMMUTABLE STRICT SET search_path = public, pg_temp
AS $$
    SELECT cardinality(hosts) <= 20
      AND (cardinality(hosts) = 0 OR array_ndims(hosts) = 1)
      AND NOT EXISTS (SELECT 1 FROM unnest(hosts) AS h WHERE h IS NULL OR NOT weave_egress_hostname_valid(h))
      AND hosts = ARRAY(SELECT DISTINCT h FROM unnest(hosts) AS h ORDER BY h);
$$;
CREATE TABLE runner_egress_snapshots (
    runner_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    session_id uuid NOT NULL,
    hosts text[] NOT NULL CHECK (weave_egress_snapshot_hosts_valid(hosts)),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT runner_egress_snapshots_runner_fkey FOREIGN KEY (runner_id, session_id, workspace_id)
        REFERENCES runners(id, session_id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX runner_egress_snapshots_workspace_idx ON runner_egress_snapshots(workspace_id);
ALTER TABLE runner_egress_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE runner_egress_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY runner_egress_snapshots_tenant_read ON runner_egress_snapshots FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
GRANT SELECT ON runner_egress_snapshots TO weave_app;

-- Existing runners never had additions. Do not fill history from current config.
INSERT INTO runner_egress_snapshots (runner_id, workspace_id, session_id, hosts)
    SELECT id, workspace_id, session_id, ARRAY[]::text[] FROM runners;

CREATE FUNCTION weave_lock_runner_egress_workspace() RETURNS trigger
LANGUAGE plpgsql SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.workspace_id IS DISTINCT FROM weave_current_workspace_id() THEN
        RAISE EXCEPTION 'runner snapshot tenant mismatch' USING ERRCODE = 'insufficient_privilege';
    END IF;
    PERFORM id FROM workspaces WHERE id = NEW.workspace_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'workspace not found' USING ERRCODE = 'foreign_key_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER runners_lock_egress_workspace BEFORE INSERT ON runners
    FOR EACH ROW EXECUTE FUNCTION weave_lock_runner_egress_workspace();

-- Narrow privileged write: no parameters, only the inserting runner's NEW row.
-- No client INSERT grant on snapshots and no callable general policy writer.
CREATE FUNCTION weave_capture_runner_egress() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.workspace_id IS DISTINCT FROM weave_current_workspace_id() THEN
        RAISE EXCEPTION 'runner snapshot tenant mismatch' USING ERRCODE = 'insufficient_privilege';
    END IF;
    INSERT INTO runner_egress_snapshots (runner_id, workspace_id, session_id, hosts)
    SELECT NEW.id, NEW.workspace_id, NEW.session_id,
        COALESCE(array_agg(hostname ORDER BY hostname), ARRAY[]::text[])
    FROM workspace_egress_hosts WHERE workspace_id = NEW.workspace_id;
    RETURN NEW;
END;
$$;
ALTER FUNCTION weave_capture_runner_egress() OWNER TO weave_rls_bypass;
GRANT SELECT ON workspace_egress_hosts TO weave_rls_bypass;
GRANT INSERT ON runner_egress_snapshots TO weave_rls_bypass;
GRANT EXECUTE ON FUNCTION weave_egress_hostname_valid(text), weave_egress_snapshot_hosts_valid(text[]) TO weave_rls_bypass;
REVOKE ALL ON FUNCTION weave_capture_runner_egress() FROM PUBLIC;
CREATE TRIGGER runners_capture_egress AFTER INSERT ON runners
    FOR EACH ROW EXECUTE FUNCTION weave_capture_runner_egress();

CREATE FUNCTION weave_egress_snapshot_reject_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path = public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1
       AND NOT EXISTS (SELECT 1 FROM runners WHERE id = OLD.runner_id) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'runner egress snapshots are immutable' USING ERRCODE = 'restrict_violation';
END;
$$;
CREATE TRIGGER runner_egress_snapshots_immutable BEFORE UPDATE OR DELETE ON runner_egress_snapshots
    FOR EACH ROW EXECUTE FUNCTION weave_egress_snapshot_reject_mutation();
CREATE TRIGGER runner_egress_snapshots_no_truncate BEFORE TRUNCATE ON runner_egress_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION weave_egress_snapshot_reject_mutation();

REVOKE ALL ON FUNCTION weave_check_egress_host_cap(), weave_lock_runner_egress_workspace(),
    weave_egress_snapshot_reject_mutation() FROM PUBLIC;
COMMENT ON TABLE runner_egress_snapshots IS 'Immutable configured additional hosts at runner allocation; guarded forwarding is wired in M5.4d.3.';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER runners_capture_egress ON runners;
DROP TRIGGER runners_lock_egress_workspace ON runners;
DROP FUNCTION weave_capture_runner_egress();
DROP FUNCTION weave_lock_runner_egress_workspace();
DROP TABLE runner_egress_snapshots;
DROP FUNCTION weave_egress_snapshot_reject_mutation();
DROP FUNCTION weave_egress_snapshot_hosts_valid(text[]);
ALTER TABLE runners DROP CONSTRAINT runners_id_session_workspace_key;
DROP TABLE workspace_egress_hosts;
DROP FUNCTION weave_check_egress_host_cap();
DROP FUNCTION weave_egress_hostname_valid(text);
-- +goose StatementEnd
