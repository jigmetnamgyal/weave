-- +goose Up
-- +goose StatementBegin

-- GitHub App installations, the repositories they grant, and the permissions
-- they actually hold.
--
-- Row-level security is enabled in this same migration rather than a later
-- one. RLS is off by default, so a tenant-owned table created without it is
-- silently unprotected and looks entirely correct — which is how it would stay
-- until someone read another tenant's rows. See ADR-012.

-- An installation of the Weave GitHub App, bound to exactly one workspace.
CREATE TABLE github_installations (
    id                     uuid        PRIMARY KEY,
    -- CASCADE: an installation belonging to a deleted workspace grants nothing
    -- to anyone. The audit trail of the connection lives in audit_events,
    -- which deliberately outlives its workspace.
    workspace_id           uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- GitHub's own identifier for the installation. This is the join key for
    -- every webhook, because a delivery names the installation and nothing
    -- else we know.
    github_installation_id bigint      NOT NULL,
    account_login          text        NOT NULL,
    account_type           text        NOT NULL,
    -- Whether the installation grants every repository in the account or a
    -- chosen subset. Recorded because "all" means the granted set changes when
    -- a repository is created, with no event naming it.
    repository_selection   text        NOT NULL,
    connected_by           uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    suspended_at           timestamptz,
    -- When GitHub reports the installation removed.
    --
    -- Recorded rather than deleting the row, because deleting it would cascade
    -- to the repositories, and those rows are what make an old audit entry or
    -- a finished session readable later. GitHub issues a new installation id
    -- for a reinstall, so keeping the old row cannot collide with one.
    deleted_at             timestamptz,
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),

    -- Unique across the whole table, not per workspace. This is the constraint
    -- that makes double-binding impossible rather than merely discouraged: an
    -- installation already attached to one workspace cannot be attached to a
    -- second, so a repository grant cannot be moved between tenants by
    -- replaying a callback.
    CONSTRAINT github_installations_installation_key
        UNIQUE (github_installation_id),
    -- Referenced by the composite foreign keys below, which is what stops a
    -- child row claiming a different workspace than its installation.
    CONSTRAINT github_installations_id_workspace_key
        UNIQUE (id, workspace_id),
    CONSTRAINT github_installations_account_type_valid
        CHECK (account_type IN ('User', 'Organization')),
    CONSTRAINT github_installations_repository_selection_valid
        CHECK (repository_selection IN ('all', 'selected')),
    CONSTRAINT github_installations_account_login_not_blank
        CHECK (length(btrim(account_login)) > 0),
    CONSTRAINT github_installations_github_id_positive
        CHECK (github_installation_id > 0)
);

CREATE INDEX github_installations_workspace_idx
    ON github_installations (workspace_id);

-- A repository the installation grants.
--
-- These rows are a cache of what GitHub told us, never the authority. GitHub
-- decides access; we record our belief about it, and belief goes stale.
CREATE TABLE repositories (
    id                   uuid        PRIMARY KEY,
    workspace_id         uuid        NOT NULL,
    installation_id      uuid        NOT NULL,
    -- GitHub's numeric repository id is the identity, not owner/name. Those
    -- change on rename and transfer, and a rename must not read as a different
    -- repository.
    github_repository_id bigint      NOT NULL,
    owner                text        NOT NULL,
    name                 text        NOT NULL,
    default_branch       text        NOT NULL,
    private              boolean     NOT NULL,
    -- Whether the installation currently grants it. Repositories are never
    -- deleted when access is withdrawn: the row is the record that we once had
    -- access, which matters for reading old audit entries and sessions.
    granted              boolean     NOT NULL DEFAULT true,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),

    -- Composite, so a repository cannot name one workspace while its
    -- installation belongs to another. Without this, workspace_id here would
    -- be an unchecked copy, and the copy is what every RLS policy reads.
    CONSTRAINT repositories_installation_fkey
        FOREIGN KEY (installation_id, workspace_id)
        REFERENCES github_installations (id, workspace_id) ON DELETE CASCADE,
    -- Scoped to the installation rather than global: an installation can be
    -- removed and the account can install again against a different workspace,
    -- and the withdrawn rows are kept.
    CONSTRAINT repositories_installation_repository_key
        UNIQUE (installation_id, github_repository_id),
    CONSTRAINT repositories_github_id_positive
        CHECK (github_repository_id > 0),
    CONSTRAINT repositories_owner_not_blank
        CHECK (length(btrim(owner)) > 0),
    CONSTRAINT repositories_name_not_blank
        CHECK (length(btrim(name)) > 0),
    CONSTRAINT repositories_default_branch_not_blank
        CHECK (length(btrim(default_branch)) > 0)
);

CREATE INDEX repositories_workspace_idx ON repositories (workspace_id);
CREATE INDEX repositories_github_id_idx ON repositories (github_repository_id);

-- The permissions the installation actually holds, as GitHub reports them.
--
-- Recorded so a health check can say which permission is missing rather than
-- only that something is wrong. An App's permission set changes over time and
-- existing installations keep the set they accepted, so what we requested is
-- not what any given installation grants.
CREATE TABLE repository_permissions (
    installation_id uuid        NOT NULL,
    workspace_id    uuid        NOT NULL,
    permission      text        NOT NULL,
    access          text        NOT NULL,
    recorded_at     timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (installation_id, permission),
    CONSTRAINT repository_permissions_installation_fkey
        FOREIGN KEY (installation_id, workspace_id)
        REFERENCES github_installations (id, workspace_id) ON DELETE CASCADE,
    CONSTRAINT repository_permissions_access_valid
        CHECK (access IN ('read', 'write', 'admin')),
    CONSTRAINT repository_permissions_permission_not_blank
        CHECK (length(btrim(permission)) > 0)
);

CREATE INDEX repository_permissions_workspace_idx
    ON repository_permissions (workspace_id);

-- Webhook deliveries, for deduplication and for answering "did we receive it".
--
-- Not workspace-owned, and deliberately so: a delivery is recorded before we
-- know which workspace it concerns, and one may name an installation we have
-- no record of at all. It carries no tenant data — an id, an event name and a
-- timestamp — so there is nothing here to scope.
CREATE TABLE github_webhook_deliveries (
    delivery_id  text        PRIMARY KEY,
    event        text        NOT NULL,
    action       text,
    received_at  timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT github_webhook_deliveries_event_not_blank
        CHECK (length(btrim(event)) > 0)
);

-- Deliveries are only interesting while deduplication needs them; this index
-- is what makes pruning old ones cheap.
CREATE INDEX github_webhook_deliveries_received_idx
    ON github_webhook_deliveries (received_at);

-- --------------------------------------------------------------------------
-- Resolving an installation without tenant context
--
-- A webhook arrives with no user and no workspace: GitHub names an
-- installation, and that is all we know. Every policy below keys on the
-- workspace, so the lookup that turns an installation id into a workspace
-- cannot itself be workspace-scoped.
--
-- This is the same shape as weave_invitation_by_token — a named SECURITY
-- DEFINER function returning one row — but the authorization differs, and the
-- difference is worth stating. A token authorizes itself: holding it is the
-- permission. An installation id does not; it is a small integer an attacker
-- could guess. What authorizes this call is the HMAC signature on the webhook
-- body, verified before the handler reaches the database at all.
--
-- So the rule that keeps this narrow is not the argument, it is the caller:
-- only the webhook handler may use it, and only after signature verification.
-- It returns the workspace and installation ids and nothing else, so the worst
-- it yields is that a given installation belongs to some workspace.
-- --------------------------------------------------------------------------
CREATE FUNCTION weave_installation_by_github_id(presented_id bigint)
RETURNS TABLE (
    installation_id uuid,
    workspace_id    uuid,
    suspended       boolean
)
LANGUAGE sql STABLE SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT i.id, i.workspace_id, i.suspended_at IS NOT NULL
    FROM github_installations i
    WHERE i.github_installation_id = presented_id;
$$;

ALTER FUNCTION weave_installation_by_github_id(bigint) OWNER TO weave_rls_bypass;

REVOKE ALL ON FUNCTION weave_installation_by_github_id(bigint) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION weave_installation_by_github_id(bigint) TO weave_app;

GRANT SELECT ON github_installations TO weave_rls_bypass;

GRANT SELECT, INSERT, UPDATE, DELETE ON
    github_installations, repositories, repository_permissions,
    github_webhook_deliveries
    TO weave_app;

-- --------------------------------------------------------------------------
-- Policies
--
-- Same shape as migration 00004: FORCE as the second layer, the application
-- connecting as weave_app as the control that actually carries isolation, and
-- an absent context matching no row.
-- --------------------------------------------------------------------------

ALTER TABLE github_installations   ENABLE ROW LEVEL SECURITY;
ALTER TABLE github_installations   FORCE  ROW LEVEL SECURITY;
ALTER TABLE repositories           ENABLE ROW LEVEL SECURITY;
ALTER TABLE repositories           FORCE  ROW LEVEL SECURITY;
ALTER TABLE repository_permissions ENABLE ROW LEVEL SECURITY;
ALTER TABLE repository_permissions FORCE  ROW LEVEL SECURITY;

-- github_installations
--
-- The membership arm mirrors the workspaces policy: connecting an installation
-- happens with a workspace in context, but listing "which of my workspaces
-- have GitHub connected" spans several.
CREATE POLICY github_installations_tenant_read ON github_installations FOR SELECT
    USING (
        workspace_id = weave_current_workspace_id()
        OR (weave_current_workspace_id() IS NULL AND weave_is_member(workspace_id))
    );

CREATE POLICY github_installations_tenant_insert ON github_installations FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY github_installations_tenant_update ON github_installations FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY github_installations_tenant_delete ON github_installations FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

-- repositories
CREATE POLICY repositories_tenant_read ON repositories FOR SELECT
    USING (workspace_id = weave_current_workspace_id());

CREATE POLICY repositories_tenant_insert ON repositories FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY repositories_tenant_update ON repositories FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY repositories_tenant_delete ON repositories FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

-- repository_permissions
CREATE POLICY repository_permissions_tenant_read ON repository_permissions FOR SELECT
    USING (workspace_id = weave_current_workspace_id());

CREATE POLICY repository_permissions_tenant_insert ON repository_permissions FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY repository_permissions_tenant_update ON repository_permissions FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY repository_permissions_tenant_delete ON repository_permissions FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

-- github_webhook_deliveries carries no tenant data and gets no policy, for the
-- same reason users does not: there is no workspace to scope it by.

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS repository_permissions_tenant_delete ON repository_permissions;
DROP POLICY IF EXISTS repository_permissions_tenant_update ON repository_permissions;
DROP POLICY IF EXISTS repository_permissions_tenant_insert ON repository_permissions;
DROP POLICY IF EXISTS repository_permissions_tenant_read ON repository_permissions;
DROP POLICY IF EXISTS repositories_tenant_delete ON repositories;
DROP POLICY IF EXISTS repositories_tenant_update ON repositories;
DROP POLICY IF EXISTS repositories_tenant_insert ON repositories;
DROP POLICY IF EXISTS repositories_tenant_read ON repositories;
DROP POLICY IF EXISTS github_installations_tenant_delete ON github_installations;
DROP POLICY IF EXISTS github_installations_tenant_update ON github_installations;
DROP POLICY IF EXISTS github_installations_tenant_insert ON github_installations;
DROP POLICY IF EXISTS github_installations_tenant_read ON github_installations;

DROP FUNCTION IF EXISTS weave_installation_by_github_id(bigint);

DROP TABLE IF EXISTS github_webhook_deliveries;
DROP TABLE IF EXISTS repository_permissions;
DROP TABLE IF EXISTS repositories;
DROP TABLE IF EXISTS github_installations;

-- +goose StatementEnd
