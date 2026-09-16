-- +goose Up
-- +goose StatementBegin

-- Sessions, their history, and the outbox that carries work out of the
-- request.
--
-- Row-level security is enabled here rather than in a later migration, for the
-- third time and the same reason: RLS is off by default, so a tenant-owned
-- table created without it is silently unprotected and looks entirely correct.
-- See ADR-012.

-- Referenced by the composite foreign key on sessions, which is what stops a
-- session pinning an agent version belonging to a different workspace.
ALTER TABLE agent_versions
    ADD CONSTRAINT agent_versions_id_workspace_key UNIQUE (id, workspace_id);

-- Referenced by the composite foreign key on sessions.
ALTER TABLE tasks
    ADD CONSTRAINT tasks_id_workspace_key UNIQUE (id, workspace_id);

-- A run of an agent against a task.
--
-- The row carries the branch intent as columns rather than a table of its own:
-- it is one-to-one with the session, written once, and never edited. What it
-- is *not* is a record that the branch exists — nothing has created it when
-- this row is written, and nothing will until M5.
CREATE TABLE sessions (
    id               uuid        PRIMARY KEY,
    workspace_id     uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    task_id          uuid        NOT NULL,
    -- The pinned version, not the agent. This is the policy snapshot: an
    -- agent_versions row is append-only and cannot be edited, so the pin
    -- already says exactly what this session runs under. A snapshot table
    -- would hold a byte-for-byte copy of a row that cannot change — not a
    -- second record of the truth but a second thing that could disagree with
    -- it. It earns a table when a session's policy can be narrower than its
    -- agent's, which is M7 and is information that exists nowhere else.
    agent_version_id uuid        NOT NULL,
    state            text        NOT NULL DEFAULT 'queued',
    -- Optimistic concurrency. Every transition states the version it observed
    -- and fails when it has moved.
    version          integer     NOT NULL DEFAULT 1,

    -- The branch intent: what M5's workflow will create, decided here because
    -- the session id is the stable identity a deterministic name derives from.
    -- M3.3 records why that matters — a name generated per attempt makes a
    -- retry create a second branch rather than finding the first.
    repository_id    uuid        NOT NULL,
    branch_name      text        NOT NULL,
    -- Empty means the repository's default branch, resolved by the workflow
    -- against GitHub. Stored empty rather than resolved now: the default can
    -- change between this row and the branch being cut, and the workflow is
    -- the thing holding a GitHub token.
    base_branch      text        NOT NULL DEFAULT '',

    -- Invariant 10: terminal session history is immutable and reopening
    -- creates a linked continuation rather than mutating what finished. The
    -- column exists now so the link has somewhere to go; nothing sets it until
    -- continuation exists.
    continues_id     uuid,

    created_by       uuid        NOT NULL REFERENCES users (id) ON DELETE RESTRICT,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),

    -- Composite, so a session cannot reference another workspace's task,
    -- agent version or repository. Without them workspace_id here would be an
    -- unchecked copy, and the copy is what every policy below reads.
    CONSTRAINT sessions_task_fkey
        FOREIGN KEY (task_id, workspace_id)
        REFERENCES tasks (id, workspace_id),
    CONSTRAINT sessions_agent_version_fkey
        FOREIGN KEY (agent_version_id, workspace_id)
        REFERENCES agent_versions (id, workspace_id),
    CONSTRAINT sessions_repository_fkey
        FOREIGN KEY (repository_id, workspace_id)
        REFERENCES repositories (id, workspace_id),
    CONSTRAINT sessions_continues_fkey
        FOREIGN KEY (continues_id, workspace_id)
        REFERENCES sessions (id, workspace_id),
    -- Referenced by the composite foreign keys on the child tables.
    CONSTRAINT sessions_id_workspace_key UNIQUE (id, workspace_id),

    -- The sixteen states from context/architecture.md. A closed set, because a
    -- state nothing recognises is a session that stalls with no way to say so.
    CONSTRAINT sessions_state_valid CHECK (state IN (
        'draft', 'queued', 'provisioning', 'running',
        'waiting_for_input', 'waiting_for_approval',
        'pausing', 'paused', 'resuming',
        'review_ready', 'finalizing', 'completed',
        'cancelling', 'cancelled', 'failed', 'expired'
    )),
    CONSTRAINT sessions_version_positive CHECK (version > 0),
    -- The branch name is validated in the domain against Git's rules; this is
    -- the bound the column carries so no path can write something unbounded.
    CONSTRAINT sessions_branch_name_length CHECK (length(branch_name) BETWEEN 1 AND 200),
    CONSTRAINT sessions_base_branch_length CHECK (length(base_branch) <= 200),
    -- A session cannot continue itself.
    CONSTRAINT sessions_continues_not_self CHECK (continues_id IS NULL OR continues_id <> id)
);

CREATE INDEX sessions_workspace_idx ON sessions (workspace_id, state);
CREATE INDEX sessions_task_idx ON sessions (task_id);
CREATE INDEX sessions_agent_version_idx ON sessions (agent_version_id);
CREATE INDEX sessions_repository_idx ON sessions (repository_id);

-- Who is in a session, and in what capacity.
--
-- Distinct from workspace membership: a workspace member is not in every
-- session, and the capacity here is about this session rather than about the
-- workspace's permission matrix.
CREATE TABLE session_participants (
    session_id   uuid        NOT NULL,
    workspace_id uuid        NOT NULL,
    user_id      uuid        NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    capacity     text        NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),

    PRIMARY KEY (session_id, user_id),
    CONSTRAINT session_participants_session_fkey
        FOREIGN KEY (session_id, workspace_id)
        REFERENCES sessions (id, workspace_id) ON DELETE CASCADE,
    -- `owner` is the person who started it; `collaborator` joined to take
    -- part; `observer` is watching. A closed set for the same reason the
    -- states are one.
    CONSTRAINT session_participants_capacity_valid
        CHECK (capacity IN ('owner', 'collaborator', 'observer'))
);

CREATE INDEX session_participants_user_idx ON session_participants (user_id);
CREATE INDEX session_participants_workspace_idx ON session_participants (workspace_id);

-- Every state a session has been in, and why.
--
-- Append-only, and that is invariant 9 stated as a table: session history is
-- append-only and ordered, and a correction is a new row rather than a rewrite
-- of an old one. Enforced by the same two triggers agent_versions carries —
-- see the comment on the function below for the cascade problem they have to
-- solve, which was worked out in M4.1 rather than here.
CREATE TABLE session_state_transitions (
    id                uuid        PRIMARY KEY,
    session_id        uuid        NOT NULL,
    workspace_id      uuid        NOT NULL,
    -- Empty on the first transition: a session comes into existence already in
    -- a state, and pretending it moved there from somewhere would be a
    -- fiction. NULL rather than a sentinel, so "no previous state" cannot be
    -- mistaken for a state.
    previous_state    text,
    next_state        text        NOT NULL,
    -- The session version the transition observed. Recorded, not just checked,
    -- so the history says which version each move was made against.
    observed_version  integer     NOT NULL,
    reason            text        NOT NULL DEFAULT '',
    -- NULL when the actor is the system rather than a person: a timeout that
    -- expires a session is not attributable to anyone, and naming a user for
    -- it would be a false statement in an append-only trail.
    actor_user_id     uuid        REFERENCES users (id) ON DELETE RESTRICT,
    created_at        timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT session_state_transitions_session_fkey
        FOREIGN KEY (session_id, workspace_id)
        REFERENCES sessions (id, workspace_id) ON DELETE CASCADE,
    CONSTRAINT session_state_transitions_previous_valid CHECK (previous_state IS NULL OR previous_state IN (
        'draft', 'queued', 'provisioning', 'running',
        'waiting_for_input', 'waiting_for_approval',
        'pausing', 'paused', 'resuming',
        'review_ready', 'finalizing', 'completed',
        'cancelling', 'cancelled', 'failed', 'expired'
    )),
    CONSTRAINT session_state_transitions_next_valid CHECK (next_state IN (
        'draft', 'queued', 'provisioning', 'running',
        'waiting_for_input', 'waiting_for_approval',
        'pausing', 'paused', 'resuming',
        'review_ready', 'finalizing', 'completed',
        'cancelling', 'cancelled', 'failed', 'expired'
    )),
    CONSTRAINT session_state_transitions_observed_version_positive CHECK (observed_version > 0),
    CONSTRAINT session_state_transitions_reason_length CHECK (length(reason) <= 500),
    -- A transition that goes nowhere is not a transition.
    CONSTRAINT session_state_transitions_moves CHECK (previous_state IS NULL OR previous_state <> next_state)
);

CREATE INDEX session_state_transitions_session_idx
    ON session_state_transitions (session_id, created_at);
CREATE INDEX session_state_transitions_workspace_idx
    ON session_state_transitions (workspace_id);

-- --------------------------------------------------------------------------
-- The outbox
--
-- A row here is the durable promise that something will happen elsewhere. It
-- is written in the same transaction as the thing that caused it, which is the
-- whole point: writing the session and then publishing has a window where the
-- session exists and nothing will ever pick it up, and that window is open
-- exactly when the process dies.
--
-- The columns follow from the outcomes, decided before the table was written
-- (context/progress-tracker.md carries the full table):
--
--   unclaimed            -> visible to the next claimer: completed_at IS NULL,
--                           available_at <= now(), no live lease
--   claimed, succeeded   -> completed_at set; pruned on a retention window,
--                           the way github_webhook_deliveries already is
--   claimed, failed      -> lease cleared, attempts incremented, available_at
--                           pushed out, last_error recorded
--   claimed, then died   -> the lease EXPIRES. This is why leased_until is a
--                           timestamp and not a boolean `claimed`: a flag set
--                           by a process that never returns is a row nothing
--                           will ever touch again, and no operator would know.
--   claimed twice at once-> the claim is one conditional UPDATE ... RETURNING,
--                           never a read followed by a write
--
-- **The publisher is not built here** (M5 builds it), so the last two outcomes
-- are designed and not exercised: nothing claims a row yet, so no test has
-- watched a lease expire or two claimers race. That gap is recorded in the
-- tracker rather than left to be discovered.
-- --------------------------------------------------------------------------
CREATE TABLE outbox_events (
    id            uuid        PRIMARY KEY,
    workspace_id  uuid        NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- What happened, and what it happened to. Stable strings: the consumer
    -- dispatches on topic, and a rename is a new topic rather than a changed
    -- meaning.
    topic         text        NOT NULL,
    subject_id    uuid        NOT NULL,
    payload       jsonb       NOT NULL DEFAULT '{}',

    attempts      integer     NOT NULL DEFAULT 0,
    -- When this row next becomes claimable. now() on insert; pushed out by
    -- backoff after a failure.
    available_at  timestamptz NOT NULL DEFAULT now(),
    -- The lease. NULL means unclaimed; a time in the past means the claimer
    -- died and the row is claimable again.
    leased_until  timestamptz,
    last_error    text,
    completed_at  timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT outbox_events_topic_not_blank CHECK (length(btrim(topic)) > 0),
    CONSTRAINT outbox_events_topic_length CHECK (length(topic) <= 100),
    CONSTRAINT outbox_events_attempts_not_negative CHECK (attempts >= 0),
    CONSTRAINT outbox_events_last_error_length CHECK (last_error IS NULL OR length(last_error) <= 2000),
    -- A payload is an object. The same reason the agent tool policy is: jsonb
    -- accepts any JSON value, and a consumer reading a string where it expects
    -- fields finds out at the worst moment.
    CONSTRAINT outbox_events_payload_is_object CHECK (jsonb_typeof(payload) = 'object')
);

-- The claim index: pending rows in the order they became available. Partial,
-- because a completed row is never claimed again and there will be far more of
-- them than pending ones.
CREATE INDEX outbox_events_claimable_idx
    ON outbox_events (available_at)
    WHERE completed_at IS NULL;

CREATE INDEX outbox_events_workspace_idx ON outbox_events (workspace_id);

-- --------------------------------------------------------------------------
-- Append-only enforcement for session_state_transitions
--
-- Both triggers, for the reason audit_events and agent_versions carry both: a
-- row trigger alone leaves the table erasable in one TRUNCATE, which fires no
-- row-level trigger at all.
-- --------------------------------------------------------------------------

CREATE FUNCTION session_state_transitions_reject_mutation() RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    -- Removing the whole session is allowed to take its history with it.
    --
    -- This is the problem M4.1 solved for agent_versions, and the reasoning
    -- carries over unchanged: append-only exists so history cannot be
    -- rewritten, and when the session — and with it the workspace — is being
    -- deleted there is no history left to protect. Refusing would make a
    -- tenant undeletable, and would break the integration harness, which
    -- cleans up by deleting workspaces.
    --
    -- Two signals are required together because either alone is weak.
    -- pg_trigger_depth() > 1 says the delete is nested inside another trigger,
    -- which a cascade is — but so would be a future trigger deleting
    -- transitions directly. The parent already being gone says this is that
    -- cascade specifically.
    IF TG_OP = 'DELETE' AND pg_trigger_depth() > 1
       AND NOT EXISTS (SELECT 1 FROM sessions WHERE id = OLD.session_id) THEN
        RETURN OLD;
    END IF;

    RAISE EXCEPTION
        'session_state_transitions is append-only: invariant 9 makes session history append-only and ordered, so a correction is a new transition rather than an edit to an old one.'
        USING ERRCODE = 'restrict_violation';
END;
$$;

CREATE TRIGGER session_state_transitions_append_only
    BEFORE UPDATE OR DELETE ON session_state_transitions
    FOR EACH ROW EXECUTE FUNCTION session_state_transitions_reject_mutation();

CREATE TRIGGER session_state_transitions_no_truncate
    BEFORE TRUNCATE ON session_state_transitions
    FOR EACH STATEMENT EXECUTE FUNCTION session_state_transitions_reject_mutation();

-- --------------------------------------------------------------------------
-- Grants and policies
--
-- Same shape as the migrations before: the application connects as weave_app,
-- which is what carries isolation; FORCE is the second layer; an absent
-- context matches no row.
--
-- session_state_transitions gets no UPDATE or DELETE policy. The triggers
-- reject both regardless, and adding policies for them would imply otherwise.
--
-- outbox_events gets UPDATE but no DELETE for weave_app: a claimer marks rows,
-- and pruning completed rows is an operator job run as the owner, like the
-- delivery log's.
-- --------------------------------------------------------------------------

-- No DELETE on sessions. Deleting one cascades to
-- session_state_transitions, and the append-only trigger *permits* that
-- cascade by design — so the grant would be a route to erasing history the
-- table exists to keep, with nothing raising an error. Invariant 10 makes
-- terminal history immutable and says reopening creates a linked continuation,
-- so the application has no reason to delete a session and should not be able
-- to. Nothing queries for one today.
--
-- This narrows one path rather than closing every one: weave_app still holds
-- DELETE on workspaces from migration 00004, and that cascade reaches the same
-- rows. Nothing deletes a workspace either, and revoking it is a change to
-- another unit's grant with its own tests to re-run, so it is recorded rather
-- than done here.
GRANT SELECT, INSERT, UPDATE ON sessions TO weave_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON session_participants TO weave_app;
GRANT SELECT, INSERT ON session_state_transitions TO weave_app;
GRANT SELECT, INSERT, UPDATE ON outbox_events TO weave_app;

ALTER TABLE sessions                  ENABLE ROW LEVEL SECURITY;
ALTER TABLE sessions                  FORCE  ROW LEVEL SECURITY;
ALTER TABLE session_participants      ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_participants      FORCE  ROW LEVEL SECURITY;
ALTER TABLE session_state_transitions ENABLE ROW LEVEL SECURITY;
ALTER TABLE session_state_transitions FORCE  ROW LEVEL SECURITY;
ALTER TABLE outbox_events             ENABLE ROW LEVEL SECURITY;
ALTER TABLE outbox_events             FORCE  ROW LEVEL SECURITY;

CREATE POLICY sessions_tenant_read ON sessions FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY sessions_tenant_insert ON sessions FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY sessions_tenant_update ON sessions FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY session_participants_tenant_read ON session_participants FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY session_participants_tenant_insert ON session_participants FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY session_participants_tenant_update ON session_participants FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY session_participants_tenant_delete ON session_participants FOR DELETE
    USING (workspace_id = weave_current_workspace_id());

CREATE POLICY session_state_transitions_tenant_read ON session_state_transitions FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY session_state_transitions_tenant_insert ON session_state_transitions FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());

CREATE POLICY outbox_events_tenant_read ON outbox_events FOR SELECT
    USING (workspace_id = weave_current_workspace_id());
CREATE POLICY outbox_events_tenant_insert ON outbox_events FOR INSERT
    WITH CHECK (workspace_id = weave_current_workspace_id());
CREATE POLICY outbox_events_tenant_update ON outbox_events FOR UPDATE
    USING (workspace_id = weave_current_workspace_id())
    WITH CHECK (workspace_id = weave_current_workspace_id());

COMMENT ON TABLE sessions IS 'A run of an agent against a task. Carries the pinned agent version, which is the policy snapshot, and the branch intent M5 acts on.';
COMMENT ON TABLE session_participants IS 'Who is in a session and in what capacity. Distinct from workspace membership.';
COMMENT ON TABLE session_state_transitions IS 'Append-only. Invariant 9: session history is append-only and ordered, so a correction is a new row.';
COMMENT ON TABLE outbox_events IS 'The durable promise that something happens elsewhere, written in the transaction that caused it. The claim is a lease with an expiry, not a flag.';

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

DROP POLICY IF EXISTS outbox_events_tenant_update ON outbox_events;
DROP POLICY IF EXISTS outbox_events_tenant_insert ON outbox_events;
DROP POLICY IF EXISTS outbox_events_tenant_read ON outbox_events;
DROP POLICY IF EXISTS session_state_transitions_tenant_insert ON session_state_transitions;
DROP POLICY IF EXISTS session_state_transitions_tenant_read ON session_state_transitions;
DROP POLICY IF EXISTS session_participants_tenant_delete ON session_participants;
DROP POLICY IF EXISTS session_participants_tenant_update ON session_participants;
DROP POLICY IF EXISTS session_participants_tenant_insert ON session_participants;
DROP POLICY IF EXISTS session_participants_tenant_read ON session_participants;
DROP POLICY IF EXISTS sessions_tenant_update ON sessions;
DROP POLICY IF EXISTS sessions_tenant_insert ON sessions;
DROP POLICY IF EXISTS sessions_tenant_read ON sessions;

DROP TRIGGER IF EXISTS session_state_transitions_no_truncate ON session_state_transitions;
DROP TRIGGER IF EXISTS session_state_transitions_append_only ON session_state_transitions;
DROP FUNCTION IF EXISTS session_state_transitions_reject_mutation();

DROP TABLE IF EXISTS outbox_events;
DROP TABLE IF EXISTS session_state_transitions;
DROP TABLE IF EXISTS session_participants;
DROP TABLE IF EXISTS sessions;

ALTER TABLE tasks DROP CONSTRAINT IF EXISTS tasks_id_workspace_key;
ALTER TABLE agent_versions DROP CONSTRAINT IF EXISTS agent_versions_id_workspace_key;

-- +goose StatementEnd
