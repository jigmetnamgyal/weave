-- +goose Up
-- +goose StatementBegin

-- Workspaces are the tenant boundary. Every tenant-owned table added from
-- here on carries workspace_id, and every query filters on it.
CREATE TABLE workspaces (
    id          uuid PRIMARY KEY,
    -- citext so slug lookup is case-insensitive without lower() at every
    -- call site, matching how users.email is handled.
    slug        citext      NOT NULL,
    name        text        NOT NULL,
    -- RESTRICT, not CASCADE: deleting a user must not silently delete the
    -- workspaces they created, which other members may still be working in.
    created_by  uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    -- Optimistic concurrency. A rename carries the version it read, and the
    -- update fails rather than overwriting a concurrent edit.
    version     integer     NOT NULL DEFAULT 1,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT workspaces_slug_key UNIQUE (slug),
    -- Lowercase, digits and internal hyphens only. Slugs appear in URLs, so
    -- the shape is constrained here rather than trusted from the caller.
    --
    -- The ::text cast is load-bearing. citext overrides ~ to be
    -- case-insensitive, so without it this pattern accepts 'UpperCaseSlug'
    -- and the constraint silently does nothing.
    CONSTRAINT workspaces_slug_format CHECK (slug::text ~ '^[a-z0-9]([a-z0-9-]*[a-z0-9])?$'),
    CONSTRAINT workspaces_slug_length CHECK (length(slug::text) BETWEEN 2 AND 64),
    CONSTRAINT workspaces_name_not_blank CHECK (length(btrim(name)) > 0),
    CONSTRAINT workspaces_version_positive CHECK (version > 0)
);

-- Membership is the authorization record. A row here, and only a row here,
-- means a user may act in a workspace.
--
-- role is a CHECK-constrained text column rather than an enum: adding a role
-- later is an ordinary migration, whereas ALTER TYPE ... ADD VALUE cannot run
-- inside a transaction with other changes.
CREATE TABLE workspace_members (
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    role         text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (workspace_id, user_id),
    CONSTRAINT workspace_members_role_valid
        CHECK (role IN ('owner', 'admin', 'developer', 'viewer'))
);

-- "Which workspaces am I in" is on the path of every authenticated request,
-- and the primary key above leads with workspace_id.
CREATE INDEX workspace_members_user_id_idx ON workspace_members (user_id);

-- Append-only record of security-relevant changes.
--
-- workspace_id deliberately carries no foreign key. Deleting a workspace must
-- not delete the evidence of what was done inside it, and a CASCADE — or even
-- a RESTRICT that blocks the delete — would tie the record's lifetime to the
-- thing it exists to outlive.
CREATE TABLE audit_events (
    id            uuid        PRIMARY KEY,
    workspace_id  uuid        NOT NULL,
    -- Nullable: some events will be attributable to the system rather than a
    -- person. No foreign key, for the same reason as workspace_id.
    actor_user_id uuid,
    action        text        NOT NULL,
    target        text,
    detail        jsonb       NOT NULL DEFAULT '{}'::jsonb,
    occurred_at   timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT audit_events_action_not_blank CHECK (length(btrim(action)) > 0)
);

CREATE INDEX audit_events_workspace_occurred_idx
    ON audit_events (workspace_id, occurred_at DESC);

-- Append-only is enforced, not merely intended. Without this an ordinary bug
-- — or an attacker with the application's own credentials — could rewrite the
-- record of what they did.
CREATE FUNCTION audit_events_reject_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER audit_events_append_only
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_reject_mutation();

-- TRUNCATE fires no row-level trigger, so a row trigger alone would leave the
-- whole table erasable in a single statement. This closes that.
CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_reject_mutation();

COMMENT ON TABLE workspaces IS 'Tenant boundary. Every tenant-owned table references this.';
COMMENT ON TABLE workspace_members IS 'Authorization record: a row here means the user may act in the workspace, at the given role.';
COMMENT ON TABLE audit_events IS 'Append-only security log. Deliberately has no foreign keys so records outlive what they describe.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TRIGGER IF EXISTS audit_events_no_truncate ON audit_events;
DROP TRIGGER IF EXISTS audit_events_append_only ON audit_events;
DROP FUNCTION IF EXISTS audit_events_reject_mutation();
DROP TABLE IF EXISTS audit_events;
DROP TABLE IF EXISTS workspace_members;
DROP TABLE IF EXISTS workspaces;

-- +goose StatementEnd
