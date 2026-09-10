-- +goose Up
-- +goose StatementBegin

-- Invitations to join a workspace.
--
-- An invitation is a capability, not a notification: holding the token is what
-- grants entry. It is therefore stored the way a credential is stored — only
-- the hash — so that read access to this table does not let anyone accept the
-- invitations in it.
CREATE TABLE workspace_invitations (
    id           uuid        PRIMARY KEY,
    -- CASCADE: an invitation to a workspace that no longer exists cannot be
    -- accepted and carries no information worth keeping. This is the opposite
    -- of audit_events, which deliberately outlives its workspace.
    workspace_id uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    email        citext      NOT NULL,
    role         text        NOT NULL,
    -- SHA-256 of the token. The token itself is returned to the issuer once
    -- and never stored, so it cannot be recovered from a database dump.
    token_hash   bytea       NOT NULL,
    invited_by   uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    accepted_at  timestamptz,
    accepted_by  uuid REFERENCES users (id) ON DELETE RESTRICT,
    revoked_at   timestamptz,
    revoked_by   uuid REFERENCES users (id) ON DELETE RESTRICT,

    CONSTRAINT workspace_invitations_token_hash_key UNIQUE (token_hash),
    CONSTRAINT workspace_invitations_role_valid
        CHECK (role IN ('owner', 'admin', 'developer', 'viewer')),
    CONSTRAINT workspace_invitations_email_not_blank
        CHECK (length(btrim(email::text)) > 0),
    CONSTRAINT workspace_invitations_token_hash_length
        CHECK (octet_length(token_hash) = 32),
    -- Terminal states are mutually exclusive: an invitation that was revoked
    -- cannot also have been accepted.
    CONSTRAINT workspace_invitations_not_both_terminal
        CHECK (accepted_at IS NULL OR revoked_at IS NULL),
    -- Who and when are recorded together or not at all, so no row can say an
    -- invitation was accepted without saying by whom.
    CONSTRAINT workspace_invitations_accepted_together
        CHECK ((accepted_at IS NULL) = (accepted_by IS NULL)),
    CONSTRAINT workspace_invitations_revoked_together
        CHECK ((revoked_at IS NULL) = (revoked_by IS NULL))
);

-- At most one outstanding invitation per address per workspace.
--
-- The predicate cannot mention now(), because an index predicate must be
-- immutable — so an expired invitation still occupies this slot. Issuing
-- handles that by revoking the expired row first, which is also the honest
-- record of what happened: the old invitation is dead, not replaced.
CREATE UNIQUE INDEX workspace_invitations_one_outstanding_idx
    ON workspace_invitations (workspace_id, email)
    WHERE accepted_at IS NULL AND revoked_at IS NULL;

-- Listing a workspace's invitations is the common read.
CREATE INDEX workspace_invitations_workspace_idx
    ON workspace_invitations (workspace_id, created_at DESC);

COMMENT ON TABLE workspace_invitations IS 'Invitations to join a workspace. Only the token hash is stored; the token is shown to the issuer once.';
COMMENT ON COLUMN workspace_invitations.token_hash IS 'SHA-256 of the invitation token. Never store the token itself.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS workspace_invitations;

-- +goose StatementEnd
