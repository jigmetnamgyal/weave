-- +goose Up
-- +goose StatementBegin

-- Row-level security: the database refuses another tenant's rows regardless
-- of what the query asked for.
--
-- Deployment ordering. The policies below take effect for any role that is
-- not a superuser and not BYPASSRLS, so an application still connecting as
-- such a role would begin reading nothing the moment this lands:
--
--   1. Ship the configuration change that points the API at APP_DATABASE_URL.
--   2. Apply this migration and run `migrate app-role`.
--
-- Where the owner is a superuser — as it is locally — step 1 is not strictly
-- required for the previous version to keep working, because a superuser
-- bypasses RLS. Do not rely on that: it is a property of one deployment, not
-- of the design.

-- The role the API connects as. Deliberately not the owner, not a superuser
-- and without BYPASSRLS — all three ignore policies, so an application
-- connecting as any of them would have policies that silently do nothing.
--
-- No password here — a credential does not belong in a migration. It is set
-- per environment by `migrate app-role`, which reads APP_DATABASE_URL.
DO $$
BEGIN
    IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'weave_app') THEN
        CREATE ROLE weave_app LOGIN;
    END IF;
END
$$;

GRANT USAGE ON SCHEMA public TO weave_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
    users, workspaces, workspace_members, workspace_invitations, audit_events
    TO weave_app;

-- goose's own bookkeeping table is never touched by the application.
REVOKE ALL ON goose_db_version FROM weave_app;

-- --------------------------------------------------------------------------
-- Tenant context
--
-- Read through helpers that return NULL when the setting is absent, so a
-- query that runs without context matches no row. RLS must fail closed; one
-- that fails open looks like protection and is not.
-- --------------------------------------------------------------------------

CREATE FUNCTION weave_current_workspace_id() RETURNS uuid
LANGUAGE sql STABLE
AS $$
    SELECT NULLIF(current_setting('app.workspace_id', true), '')::uuid;
$$;

CREATE FUNCTION weave_current_user_id() RETURNS uuid
LANGUAGE sql STABLE
AS $$
    SELECT NULLIF(current_setting('app.user_id', true), '')::uuid;
$$;

-- SECURITY DEFINER so the check runs as the owner and therefore bypasses RLS.
-- Without that, a policy on workspace_members that queries workspace_members
-- recurses. It discloses nothing: it answers only about the current user.
CREATE FUNCTION weave_is_member(target_workspace uuid) RETURNS boolean
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT EXISTS (
        SELECT 1 FROM workspace_members
        WHERE workspace_id = target_workspace
          AND user_id = weave_current_user_id()
    );
$$;

-- The one deliberate hole, and it is kept to a single row.
--
-- Accepting an invitation is the only operation available to someone who is
-- not a member of anything: the token is the authorization, so there is no
-- workspace context to scope by. This returns exactly the row whose token
-- hash was presented and nothing else, so holding it cannot be widened into
-- reading a tenant's invitations.
CREATE FUNCTION weave_invitation_by_token(presented_hash bytea)
RETURNS SETOF workspace_invitations
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT * FROM workspace_invitations WHERE token_hash = presented_hash;
$$;

GRANT EXECUTE ON FUNCTION
    weave_current_workspace_id(), weave_current_user_id(),
    weave_is_member(uuid), weave_invitation_by_token(bytea)
    TO weave_app;

-- --------------------------------------------------------------------------
-- Policies
--
-- FORCE, not merely ENABLE.
--
-- Be precise about what this buys. FORCE subjects a table's *owner* to its
-- policies; it does nothing about a superuser or a role with BYPASSRLS, both
-- of which ignore RLS entirely. The local `weave` role is a superuser — the
-- postgres image creates POSTGRES_USER that way — so it still reads every row
-- here, and that is why migrations and the readiness probe can keep using it.
--
-- The control that actually carries tenant isolation is therefore the
-- application connecting as weave_app, which is neither superuser nor
-- BYPASSRLS. FORCE is the belt to that braces: it means an environment whose
-- owner is *not* a superuser also gets policies applied rather than silently
-- bypassed.
-- --------------------------------------------------------------------------

ALTER TABLE workspaces             ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces             FORCE  ROW LEVEL SECURITY;
ALTER TABLE workspace_members      ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_members      FORCE  ROW LEVEL SECURITY;
ALTER TABLE workspace_invitations  ENABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_invitations  FORCE  ROW LEVEL SECURITY;
ALTER TABLE audit_events           ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events           FORCE  ROW LEVEL SECURITY;

-- workspaces
--
-- The second arm serves "list the workspaces I belong to", which spans every
-- tenant the user is in and so has no single workspace context.
CREATE POLICY workspaces_tenant_read ON workspaces FOR SELECT
    USING (
        id = weave_current_workspace_id()
        OR (weave_current_workspace_id() IS NULL AND weave_is_member(id))
    );

-- Creating a workspace happens before any membership exists, so the
-- membership arm above cannot authorise it. You may create one you own.
CREATE POLICY workspaces_tenant_insert ON workspaces FOR INSERT
    WITH CHECK (created_by = weave_current_user_id());

CREATE POLICY workspaces_tenant_update ON workspaces FOR UPDATE
    USING (id = weave_current_workspace_id())
    WITH CHECK (id = weave_current_workspace_id());

CREATE POLICY workspaces_tenant_delete ON workspaces FOR DELETE
    USING (id = weave_current_workspace_id());

-- workspace_members
--
-- The second arm covers both listing your own memberships and inserting the
-- row that makes you a member — creating a workspace, and accepting an
-- invitation, both add a row for yourself before any context exists.
CREATE POLICY members_tenant_read ON workspace_members FOR SELECT
    USING (
        workspace_id = weave_current_workspace_id()
        OR (weave_current_workspace_id() IS NULL AND user_id = weave_current_user_id())
    );

CREATE POLICY members_tenant_insert ON workspace_members FOR INSERT
    WITH CHECK (
        workspace_id = weave_current_workspace_id()
        OR user_id = weave_current_user_id()
    );

CREATE POLICY members_tenant_update ON workspace_members FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY members_tenant_delete ON workspace_members FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

-- workspace_invitations
--
-- Reads and writes are workspace-scoped. The one exception, looking an
-- invitation up by token, goes through weave_invitation_by_token above.
--
-- Claiming an invitation happens with no workspace context, so the membership
-- arm carries it: acceptance inserts the membership first, which makes
-- weave_is_member true within the same transaction.
CREATE POLICY invitations_tenant_read ON workspace_invitations FOR SELECT
    USING (workspace_id = weave_current_workspace_id() OR weave_is_member(workspace_id));

CREATE POLICY invitations_tenant_insert ON workspace_invitations FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY invitations_tenant_update ON workspace_invitations FOR UPDATE
    USING (workspace_id = weave_current_workspace_id() OR weave_is_member(workspace_id))
    WITH CHECK (workspace_id = weave_current_workspace_id() OR weave_is_member(workspace_id));

-- audit_events
--
-- Append-only already, so no update or delete policy: the triggers reject
-- those regardless, and adding policies for them would imply otherwise.
--
-- The membership arm covers events written while no workspace context exists
-- — workspace creation and invitation acceptance both write their audit row
-- after inserting the membership that makes weave_is_member true.
CREATE POLICY audit_tenant_read ON audit_events FOR SELECT
    USING (workspace_id = weave_current_workspace_id() OR weave_is_member(workspace_id));

CREATE POLICY audit_tenant_insert ON audit_events FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id() OR weave_is_member(workspace_id));

-- users is not tenant-owned: a user exists independently of any workspace,
-- and provisioning happens before membership. No policy.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS audit_tenant_insert ON audit_events;
DROP POLICY IF EXISTS audit_tenant_read ON audit_events;
DROP POLICY IF EXISTS invitations_tenant_update ON workspace_invitations;
DROP POLICY IF EXISTS invitations_tenant_insert ON workspace_invitations;
DROP POLICY IF EXISTS invitations_tenant_read ON workspace_invitations;
DROP POLICY IF EXISTS members_tenant_delete ON workspace_members;
DROP POLICY IF EXISTS members_tenant_update ON workspace_members;
DROP POLICY IF EXISTS members_tenant_insert ON workspace_members;
DROP POLICY IF EXISTS members_tenant_read ON workspace_members;
DROP POLICY IF EXISTS workspaces_tenant_delete ON workspaces;
DROP POLICY IF EXISTS workspaces_tenant_update ON workspaces;
DROP POLICY IF EXISTS workspaces_tenant_insert ON workspaces;
DROP POLICY IF EXISTS workspaces_tenant_read ON workspaces;

ALTER TABLE audit_events           DISABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_invitations  DISABLE ROW LEVEL SECURITY;
ALTER TABLE workspace_members      DISABLE ROW LEVEL SECURITY;
ALTER TABLE workspaces             DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS weave_invitation_by_token(bytea);
DROP FUNCTION IF EXISTS weave_is_member(uuid);
DROP FUNCTION IF EXISTS weave_current_user_id();
DROP FUNCTION IF EXISTS weave_current_workspace_id();

-- The role is left in place: it may own grants elsewhere, and dropping a role
-- that another environment still uses is not something a rollback should do.

-- +goose StatementEnd
