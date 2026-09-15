-- name: CreateSession :one
INSERT INTO sessions (
    id, workspace_id, task_id, agent_version_id, state,
    repository_id, branch_name, base_branch, created_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: AddSessionParticipant :one
INSERT INTO session_participants (session_id, workspace_id, user_id, capacity)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: AppendSessionTransition :one
-- Append-only, enforced by trigger as well as by there being no other
-- statement that touches this table.
INSERT INTO session_state_transitions (
    id, session_id, workspace_id, previous_state, next_state,
    observed_version, reason, actor_user_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetSessionForWorkspace :one
-- Scoped by workspace, so a session id from one tenant cannot be read through
-- another even before the policies are consulted.
SELECT * FROM sessions
WHERE id = $1 AND workspace_id = $2;

-- name: ListSessionsForWorkspace :many
SELECT * FROM sessions
WHERE workspace_id = $1
ORDER BY created_at DESC;

-- name: ListSessionParticipants :many
SELECT * FROM session_participants
WHERE session_id = $1 AND workspace_id = $2
ORDER BY created_at;

-- name: ListSessionTransitions :many
SELECT * FROM session_state_transitions
WHERE session_id = $1 AND workspace_id = $2
ORDER BY created_at, id;

-- name: EnqueueOutboxEvent :one
-- Written in the same transaction as the thing it promises. That is the whole
-- mechanism: writing the session and then publishing leaves a window where the
-- session exists and nothing will pick it up, open exactly when the process
-- dies.
INSERT INTO outbox_events (id, workspace_id, topic, subject_id, payload)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: ListPendingOutboxEvents :many
-- Reads what a claimer would find, without claiming it.
--
-- `leased_until` is a lease and not a flag: a row whose lease has expired is
-- pending again, because the publisher holding it died and no operator is
-- going to notice a boolean nobody will ever clear. The claim itself lands in
-- M5 as a single conditional UPDATE, so that two claimers cannot both win.
SELECT * FROM outbox_events
WHERE workspace_id = $1
  AND completed_at IS NULL
  AND available_at <= now()
  AND (leased_until IS NULL OR leased_until < now())
ORDER BY available_at, id;
