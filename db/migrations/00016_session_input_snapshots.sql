-- +goose Up
-- +goose StatementBegin

-- Additive: old binaries keep their INSERT syntax. Never reconstruct legacy
-- session input from today's mutable task; absence must fail closed at activation.
CREATE TABLE session_input_snapshots (
    session_id uuid PRIMARY KEY,
    workspace_id uuid NOT NULL,
    input_version integer NOT NULL DEFAULT 1 CHECK (input_version = 1),
    task_title text NOT NULL CHECK (length(btrim(task_title)) > 0 AND length(task_title) <= 200),
    task_body text NOT NULL CHECK (length(task_body) <= 50000),
    created_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT session_input_snapshots_session_fkey FOREIGN KEY (session_id, workspace_id)
        REFERENCES sessions(id, workspace_id) ON DELETE CASCADE
);
CREATE INDEX session_input_snapshots_workspace_idx ON session_input_snapshots(workspace_id);
ALTER TABLE session_input_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_input_snapshots FORCE ROW LEVEL SECURITY;
CREATE POLICY session_input_snapshots_tenant_read ON session_input_snapshots FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
GRANT SELECT ON session_input_snapshots TO weave_app;

-- Invoker lock: no new privileged UPDATE grant on tasks is needed for locking.
-- Held until the session/snapshot/outbox transaction commits or rolls back.
CREATE FUNCTION weave_lock_session_input_task() RETURNS trigger
LANGUAGE plpgsql SET search_path = public, pg_temp
AS $$
DECLARE
    task_status text;
    task_repository uuid;
BEGIN
    IF NEW.workspace_id IS DISTINCT FROM weave_current_workspace_id() THEN
        RAISE EXCEPTION 'session input tenant mismatch' USING ERRCODE = 'insufficient_privilege';
    END IF;
    SELECT status, repository_id INTO task_status, task_repository
        FROM tasks WHERE id = NEW.task_id AND workspace_id = NEW.workspace_id
        FOR NO KEY UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'session input task missing' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'sessions_task_fkey';
    END IF;
    IF task_status <> 'ready' THEN
        RAISE EXCEPTION 'session input task not runnable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'session_input_task_not_runnable';
    END IF;
    IF task_repository IS DISTINCT FROM NEW.repository_id THEN
        -- Preserve the existing not-found API contract for unreachable refs.
        IF NOT EXISTS (SELECT 1 FROM repositories WHERE id = NEW.repository_id AND workspace_id = NEW.workspace_id) THEN
            RAISE EXCEPTION 'session input repository missing'
                USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'sessions_repository_fkey';
        END IF;
        RAISE EXCEPTION 'session input task not runnable'
            USING ERRCODE = 'check_violation', CONSTRAINT = 'session_input_task_not_runnable';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER sessions_lock_input_task BEFORE INSERT ON sessions
    FOR EACH ROW EXECUTE FUNCTION weave_lock_session_input_task();

-- Narrow privileged write, no parameters. The invoker trigger already locked
-- this tenant's exact task. Append-only agent_versions remain the settings truth.
CREATE FUNCTION weave_capture_session_input() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.workspace_id IS DISTINCT FROM weave_current_workspace_id() THEN
        RAISE EXCEPTION 'session input tenant mismatch' USING ERRCODE = 'insufficient_privilege';
    END IF;
    INSERT INTO session_input_snapshots (session_id, workspace_id, task_title, task_body)
        SELECT NEW.id, NEW.workspace_id, title, body FROM tasks
        WHERE id = NEW.task_id AND workspace_id = NEW.workspace_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'session input task missing' USING ERRCODE = 'foreign_key_violation', CONSTRAINT = 'sessions_task_fkey';
    END IF;
    RETURN NEW;
END;
$$;
ALTER FUNCTION weave_capture_session_input() OWNER TO weave_rls_bypass;
GRANT SELECT ON tasks TO weave_rls_bypass;
GRANT INSERT ON session_input_snapshots TO weave_rls_bypass;
REVOKE ALL ON FUNCTION weave_capture_session_input() FROM PUBLIC;
CREATE TRIGGER sessions_capture_input AFTER INSERT ON sessions
    FOR EACH ROW EXECUTE FUNCTION weave_capture_session_input();

CREATE FUNCTION weave_session_input_reject_mutation() RETURNS trigger
LANGUAGE plpgsql SET search_path = public, pg_temp
AS $$
BEGIN
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1
       AND NOT EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) THEN
        RETURN OLD;
    END IF;
    RAISE EXCEPTION 'session input snapshots are immutable' USING ERRCODE = 'restrict_violation';
END;
$$;
CREATE TRIGGER session_input_snapshots_immutable BEFORE UPDATE OR DELETE ON session_input_snapshots
    FOR EACH ROW EXECUTE FUNCTION weave_session_input_reject_mutation();
CREATE TRIGGER session_input_snapshots_no_truncate BEFORE TRUNCATE ON session_input_snapshots
    FOR EACH STATEMENT EXECUTE FUNCTION weave_session_input_reject_mutation();

-- A settings pin is only immutable if the session cannot point at another one.
-- State/version/updated_at and the existing write-once branch_sha stay writable.
CREATE FUNCTION weave_session_input_identity_immutable() RETURNS trigger
LANGUAGE plpgsql SET search_path = public, pg_temp
AS $$
BEGIN
    IF ROW(NEW.id, NEW.workspace_id, NEW.task_id, NEW.agent_version_id,
           NEW.repository_id, NEW.branch_name, NEW.base_branch, NEW.continues_id,
           NEW.created_by, NEW.created_at)
       IS DISTINCT FROM
       ROW(OLD.id, OLD.workspace_id, OLD.task_id, OLD.agent_version_id,
           OLD.repository_id, OLD.branch_name, OLD.base_branch, OLD.continues_id,
           OLD.created_by, OLD.created_at) THEN
        RAISE EXCEPTION 'session input identity is immutable' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER sessions_input_identity_immutable BEFORE UPDATE ON sessions
    FOR EACH ROW EXECUTE FUNCTION weave_session_input_identity_immutable();
REVOKE ALL ON FUNCTION weave_lock_session_input_task(), weave_session_input_reject_mutation(),
    weave_session_input_identity_immutable() FROM PUBLIC;
COMMENT ON TABLE session_input_snapshots IS 'Task text captured at session INSERT. Legacy sessions have no row; never substitute current task text. No provider credentials.';

-- +goose StatementEnd
-- +goose Down
-- +goose StatementBegin
DROP TRIGGER sessions_input_identity_immutable ON sessions;
DROP FUNCTION weave_session_input_identity_immutable();
DROP TRIGGER sessions_capture_input ON sessions;
DROP FUNCTION weave_capture_session_input();
DROP TRIGGER sessions_lock_input_task ON sessions;
DROP FUNCTION weave_lock_session_input_task();
DROP TABLE session_input_snapshots;
DROP FUNCTION weave_session_input_reject_mutation();
-- +goose StatementEnd
