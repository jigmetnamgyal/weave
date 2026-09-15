-- +goose Up
-- +goose StatementBegin

-- Tasks and versioned agent profiles: the records a session is created from.
--
-- Row-level security is enabled here rather than in a later migration. RLS is
-- off by default, so a tenant-owned table created without it is silently
-- unprotected and looks entirely correct — which is how it would stay until
-- someone read another tenant's rows. See ADR-012.

-- Referenced by the composite foreign key on tasks, which is what stops a task
-- naming a repository belonging to a different workspace.
ALTER TABLE repositories
    ADD CONSTRAINT repositories_id_workspace_key UNIQUE (id, workspace_id);

-- A unit of work someone wants done.
CREATE TABLE tasks (
    id            uuid        PRIMARY KEY,
    workspace_id  uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- Nullable while a task is still being written: a repository is not chosen
    -- at the moment someone starts describing what they want. It becomes
    -- required at `ready`, enforced below.
    repository_id uuid,
    title         text        NOT NULL,
    -- Untrusted input. Stored and returned as data, never interpolated into a
    -- command, a prompt template, a ref name or a format string.
    body          text        NOT NULL DEFAULT '',
    status        text        NOT NULL DEFAULT 'draft',
    created_by    uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),

    -- Composite, so a task cannot reference another workspace's repository.
    -- Without it, workspace_id here would be an unchecked copy — and the copy
    -- is what every policy below reads.
    --
    -- NO ACTION, which is the default and is left implicit. `SET NULL
    -- (repository_id)` was tried first and is wrong: it works for a draft but
    -- a ready task's new NULL violates tasks_ready_has_repository below, so
    -- the deletion aborts anyway — measured, deleting an installation raised
    -- `new row for relation "tasks" violates check constraint`. That is the
    -- same refusal NO ACTION gives, arrived at by accident and reported
    -- against a row the caller was not touching. Nothing deletes a repository
    -- today (a withdrawn grant sets `granted = false` and an uninstall sets
    -- `deleted_at`), so the choice costs nothing now and forces whoever writes
    -- that path later to decide what happens to the tasks rather than inherit
    -- a guess. Deleting a workspace still cascades: the task row is gone
    -- before the constraint is checked, verified against PostgreSQL.
    CONSTRAINT tasks_repository_fkey
        FOREIGN KEY (repository_id, workspace_id)
        REFERENCES repositories (id, workspace_id),
    CONSTRAINT tasks_status_valid
        CHECK (status IN ('draft', 'ready', 'archived')),
    -- A task cannot be ready to run without naming what it runs against.
    -- Stated as a constraint rather than a handler check, so no path can write
    -- a row that a session would later fail on.
    CONSTRAINT tasks_ready_has_repository
        CHECK (status <> 'ready' OR repository_id IS NOT NULL),
    CONSTRAINT tasks_title_not_blank
        CHECK (length(btrim(title)) > 0),
    -- Bounded at the database, not only in the handler, so a path that skips
    -- validation still cannot write something unbounded.
    CONSTRAINT tasks_title_length
        CHECK (length(title) <= 200),
    CONSTRAINT tasks_body_length
        CHECK (length(body) <= 50000)
);

CREATE INDEX tasks_workspace_idx ON tasks (workspace_id, status);
CREATE INDEX tasks_repository_idx ON tasks (repository_id);

-- An agent profile: identity, and a pointer at the settings currently in use.
--
-- The settings themselves live in agent_versions. This row carries only what
-- is safe to change without rewriting history.
CREATE TABLE agents (
    id                 uuid        PRIMARY KEY,
    workspace_id       uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    name               text        NOT NULL,
    -- Set once the first version exists. The foreign key is added after
    -- agent_versions is created, since the two reference each other.
    current_version_id uuid,
    created_by         uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT agents_workspace_name_key UNIQUE (workspace_id, name),
    -- Referenced by the composite foreign key on agent_versions.
    CONSTRAINT agents_id_workspace_key UNIQUE (id, workspace_id),
    CONSTRAINT agents_name_not_blank CHECK (length(btrim(name)) > 0),
    CONSTRAINT agents_name_length CHECK (length(name) <= 80)
);

CREATE INDEX agents_workspace_idx ON agents (workspace_id);

-- The settings an agent ran under, frozen.
--
-- Append-only, and that is the point of the table existing at all. Invariants
-- 9 and 10 make session history append-only and terminal history immutable,
-- which cannot hold if the profile a session ran under can be edited
-- afterwards: change a model or a tool policy and every finished session
-- silently claims settings it never saw, with nothing recording that it
-- happened. Editing writes a new version; M4.2 pins a session to the version
-- id rather than the agent id.
CREATE TABLE agent_versions (
    id           uuid        PRIMARY KEY,
    agent_id     uuid        NOT NULL,
    workspace_id uuid        NOT NULL,
    version      integer     NOT NULL,
    provider     text        NOT NULL,
    model        text        NOT NULL,
    -- A closed set, checked below. Free text here would make an unrecognised
    -- capability a typo that silently disables a feature rather than a
    -- rejected write.
    capabilities text[]      NOT NULL DEFAULT '{}',
    -- What the agent is allowed to do. Opaque to this unit and interpreted in
    -- M7, but versioned here because it is a security control and a session
    -- must be readable against the policy it actually ran under.
    tool_policy  jsonb       NOT NULL DEFAULT '{}',
    created_by   uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT agent_versions_agent_fkey
        FOREIGN KEY (agent_id, workspace_id)
        REFERENCES agents (id, workspace_id) ON DELETE CASCADE,
    -- Referenced by the current-version pointer on agents.
    CONSTRAINT agent_versions_agent_id_key UNIQUE (agent_id, id),
    CONSTRAINT agent_versions_agent_version_key UNIQUE (agent_id, version),
    CONSTRAINT agent_versions_version_positive CHECK (version > 0),
    -- `fake` is not a placeholder to remove later: the session notes require a
    -- deterministic adapter to verify orchestration before a paid provider is
    -- wired in, so it is a first-class provider.
    CONSTRAINT agent_versions_provider_valid
        CHECK (provider IN ('claude_code', 'codex', 'fake')),
    CONSTRAINT agent_versions_model_not_blank
        CHECK (length(btrim(model)) > 0),
    -- The closed set. Named after the adapter contract in
    -- context/architecture.md, so a capability here corresponds to a method
    -- an adapter either implements or does not.
    CONSTRAINT agent_versions_capabilities_known
        CHECK (capabilities <@ ARRAY[
            'pause', 'resume', 'cancel', 'send_instruction',
            'structured_tool_calls', 'token_accounting'
        ]::text[])
);

CREATE INDEX agent_versions_agent_idx ON agent_versions (agent_id, version DESC);
CREATE INDEX agent_versions_workspace_idx ON agent_versions (workspace_id);

-- The pointer, added now that both tables exist.
--
-- Keyed on the agent rather than on the workspace. `(current_version_id,
-- workspace_id)` was the first version and is too weak: it stops an agent
-- pointing at another tenant's version but allows it to point at a sibling
-- agent's, which would make the profile report settings belonging to a
-- different agent entirely. Pointing at the owning agent's version subsumes
-- the workspace check, since agent_versions_agent_fkey already binds a
-- version's workspace to its agent's.
--
-- The NULL case still works: an agent has no version between its own insert
-- and its first version's, and MATCH SIMPLE — the default — does not check a
-- composite key when one of its columns is NULL.
--
-- RESTRICT rather than CASCADE: a version is never deleted, and if one somehow
-- were, losing the agent with it would be worse than the error.
ALTER TABLE agents
    ADD CONSTRAINT agents_current_version_fkey
    FOREIGN KEY (id, current_version_id)
    REFERENCES agent_versions (agent_id, id) ON DELETE RESTRICT;

-- --------------------------------------------------------------------------
-- Append-only enforcement for agent_versions
--
-- The same two triggers audit_events carries, and for the same reason: a row
-- trigger alone leaves the table erasable in one TRUNCATE, which fires no
-- row-level trigger at all.
-- --------------------------------------------------------------------------

CREATE FUNCTION agent_versions_reject_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- Removing the whole agent is allowed to take its versions with it.
    --
    -- Append-only exists so a session's history cannot be rewritten. When the
    -- agent — and with it the workspace and its sessions — is being deleted,
    -- there is no history left to protect, and refusing would make a tenant
    -- undeletable. The integration harness deletes workspaces to clean up, so
    -- refusing here would also break every test that creates a version.
    --
    -- Two signals are required together, because either alone is weak.
    -- `pg_trigger_depth() > 1` says the delete is nested inside another
    -- trigger, which a cascade is — but so would be a future trigger that
    -- deleted versions directly. The parent already being gone says this is
    -- that cascade specifically. Both were verified against PostgreSQL rather
    -- than assumed: a direct delete reports depth 1 with the agent present, a
    -- cascade reports depth 2 with it absent.
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1
       AND NOT EXISTS (SELECT 1 FROM agents WHERE id = OLD.agent_id) THEN
        RETURN OLD;
    END IF;

    RAISE EXCEPTION
        'agent_versions is append-only: a session is pinned to the version it ran under, and editing one would rewrite what finished sessions claim to have done. Create a new version instead.'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER agent_versions_append_only
    BEFORE UPDATE OR DELETE ON agent_versions
    FOR EACH ROW EXECUTE FUNCTION agent_versions_reject_mutation();

CREATE TRIGGER agent_versions_no_truncate
    BEFORE TRUNCATE ON agent_versions
    FOR EACH STATEMENT EXECUTE FUNCTION agent_versions_reject_mutation();

-- --------------------------------------------------------------------------
-- Grants and policies
--
-- Same shape as the migrations before: the application connects as weave_app,
-- which is what carries isolation; FORCE is the second layer; an absent
-- context matches no row.
--
-- agent_versions gets no UPDATE or DELETE policy. The triggers reject both
-- regardless, and adding policies for them would imply otherwise.
-- --------------------------------------------------------------------------

GRANT SELECT, INSERT, UPDATE, DELETE ON tasks, agents TO weave_app;
GRANT SELECT, INSERT ON agent_versions TO weave_app;

ALTER TABLE tasks          ENABLE ROW LEVEL SECURITY;
ALTER TABLE tasks          FORCE  ROW LEVEL SECURITY;
ALTER TABLE agents         ENABLE ROW LEVEL SECURITY;
ALTER TABLE agents         FORCE  ROW LEVEL SECURITY;
ALTER TABLE agent_versions ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_versions FORCE  ROW LEVEL SECURITY;

CREATE POLICY tasks_tenant_read ON tasks FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY tasks_tenant_insert ON tasks FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY tasks_tenant_update ON tasks FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY tasks_tenant_delete ON tasks FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

CREATE POLICY agents_tenant_read ON agents FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY agents_tenant_insert ON agents FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY agents_tenant_update ON agents FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY agents_tenant_delete ON agents FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

CREATE POLICY agent_versions_tenant_read ON agent_versions FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY agent_versions_tenant_insert ON agent_versions FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

COMMENT ON TABLE tasks IS 'A unit of work someone wants done. The body is untrusted input: stored and returned as data, never interpolated.';
COMMENT ON TABLE agents IS 'Agent identity and the pointer to the settings currently in use. Editable; the settings are not.';
COMMENT ON TABLE agent_versions IS 'Append-only. A session is pinned to a version, so editing one would rewrite what finished sessions claim to have done.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS agent_versions_tenant_insert ON agent_versions;
DROP POLICY IF EXISTS agent_versions_tenant_read ON agent_versions;
DROP POLICY IF EXISTS agents_tenant_delete ON agents;
DROP POLICY IF EXISTS agents_tenant_update ON agents;
DROP POLICY IF EXISTS agents_tenant_insert ON agents;
DROP POLICY IF EXISTS agents_tenant_read ON agents;
DROP POLICY IF EXISTS tasks_tenant_delete ON tasks;
DROP POLICY IF EXISTS tasks_tenant_update ON tasks;
DROP POLICY IF EXISTS tasks_tenant_insert ON tasks;
DROP POLICY IF EXISTS tasks_tenant_read ON tasks;

DROP TRIGGER IF EXISTS agent_versions_no_truncate ON agent_versions;
DROP TRIGGER IF EXISTS agent_versions_append_only ON agent_versions;
DROP FUNCTION IF EXISTS agent_versions_reject_mutation();

ALTER TABLE agents DROP CONSTRAINT IF EXISTS agents_current_version_fkey;

DROP TABLE IF EXISTS agent_versions;
DROP TABLE IF EXISTS agents;
DROP TABLE IF EXISTS tasks;

ALTER TABLE repositories DROP CONSTRAINT IF EXISTS repositories_id_workspace_key;

-- +goose StatementEnd
