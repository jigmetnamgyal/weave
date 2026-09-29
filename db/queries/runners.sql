-- name: CreateRunner :one
-- Fails on runners_one_live_per_session if the session already has a live
-- runner; the caller reads that one instead of making a second.
INSERT INTO runners (id, session_id, workspace_id, backend)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetLiveRunnerForSession :one
SELECT * FROM runners
WHERE session_id = $1 AND workspace_id = $2
  AND state IN ('provisioning', 'running', 'terminating');

-- name: GetRunner :one
SELECT * FROM runners
WHERE id = $1 AND workspace_id = $2;

-- name: SetRunnerHandle :one
-- Recorded as soon as the backend returns one, so a crash after this point
-- leaves something to tear down rather than an environment nothing names.
UPDATE runners
SET backend_handle = $3, updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND state IN ('provisioning', 'running', 'terminating')
RETURNING *;

-- name: MarkRunnerRunning :one
UPDATE runners
SET state = 'running', ready_at = now(), updated_at = now()
WHERE id = $1 AND workspace_id = $2 AND state = 'provisioning'
RETURNING *;

-- name: MarkRunnerTerminating :one
-- Taken before the backend is asked to destroy anything, so a crash mid-
-- teardown leaves a runner the reconciler knows to finish. updated_at marks
-- only the move into terminating: the reconciler bounds how long it leaves a
-- halted runner by it, and a retried teardown must not restart that clock.
UPDATE runners
SET state = 'terminating',
    updated_at = CASE WHEN state = 'terminating' THEN updated_at ELSE now() END
WHERE id = $1 AND workspace_id = $2 AND state IN ('provisioning', 'running', 'terminating')
RETURNING *;

-- name: EndRunner :one
-- The terminal write, after the backend confirms the environment is gone.
-- Conditional on the runner being live, so a redelivered teardown finds no
-- row and treats that as already done.
UPDATE runners
SET state = sqlc.arg(end_state), failure_reason = sqlc.narg(failure_reason),
    terminated_at = now(), updated_at = now()
WHERE id = sqlc.arg(id) AND workspace_id = sqlc.arg(workspace_id)
  AND state IN ('provisioning', 'running', 'terminating')
RETURNING *;

-- name: RunnerBoundToSession :one
-- Whether a producer is a live runner of this session. Read inside the event
-- append transaction, without a lock, after the event counter is taken.
--
-- `terminating` counts. Teardown marks the runner terminating *before* it
-- asks the backend to destroy anything, so a runner's last events — flushed
-- as it is stopped — arrive while its row reads terminating. They are that
-- session's history, from that session's runner; refusing them would record
-- a normal shutdown as an intrusion. Once the backend confirms the
-- environment is gone the row reads terminated, and nothing more is accepted.
SELECT EXISTS (
    SELECT 1 FROM runners
    WHERE id = $1 AND session_id = $2 AND state IN ('provisioning', 'running', 'terminating')
);
