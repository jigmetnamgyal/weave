-- name: NextSessionEventSequence :one
-- Takes the next number for a session, creating its counter on first use.
--
-- The upsert locks the counter row until the transaction ends, which is what
-- serialises two ingestor replicas on one session. If the insert that follows
-- is a duplicate, the caller rolls back, and the number goes with it.
INSERT INTO session_event_sequences (session_id, workspace_id, last_sequence)
VALUES ($1, $2, 1)
ON CONFLICT (session_id) DO UPDATE
    SET last_sequence = session_event_sequences.last_sequence + 1
RETURNING last_sequence;

-- name: AppendSessionEvent :one
-- DO NOTHING on the deduplication key, so a duplicate returns no row rather
-- than an error — the caller distinguishes it without parsing a message.
INSERT INTO session_events (
    session_id, sequence, workspace_id, runner_id, event_id, type,
    schema_version, correlation_id, occurred_at, received_at, payload
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
ON CONFLICT ON CONSTRAINT session_events_dedup_key DO NOTHING
RETURNING sequence;

-- name: QuarantineSessionEvent :exec
INSERT INTO session_event_quarantine (
    subject, reason, event_id, session_id, workspace_id, schema_version,
    size_bytes, delivery_count, payload_sha256
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListSessionEvents :many
-- For tests and, later, the session room. Scoped by workspace as well as the
-- policy, and ordered by the sequence — never by occurred_at, which is a claim.
SELECT * FROM session_events
WHERE session_id = $1 AND workspace_id = $2
ORDER BY sequence;

-- name: SessionStateForEvent :one
-- The session's state, read inside the append transaction **after** the
-- counter row is locked — never under a lock on the session row itself.
--
-- An earlier revision took FOR SHARE on the session here. Measured under
-- continuous output, it starved transitions: new share lockers kept jumping a
-- waiting FOR UPDATE, and a terminal transition waited the whole three-second
-- run. The counter row is the serialisation point instead (see
-- LockSessionEventSequence): a terminal transition locks it too, so either
-- the transition commits first and this read — a fresh snapshot, since the
-- lock was taken by an earlier statement — sees it, or the append commits
-- first and the transition waits for exactly that one append.
SELECT state FROM sessions
WHERE id = $1 AND workspace_id = $2;

-- name: LockSessionEventSequence :exec
-- Taken by a transition into a terminal state, before it writes the state.
--
-- Creates the counter if the session has emitted nothing yet, so the lock
-- always has a row to take. Exclusive, like every append's increment, so the
-- two queue fairly: a terminal transition waits for at most the appends
-- already holding or queued for the row, never for an unbounded stream.
INSERT INTO session_event_sequences (session_id, workspace_id, last_sequence)
VALUES ($1, $2, 0)
ON CONFLICT (session_id) DO UPDATE
    SET last_sequence = session_event_sequences.last_sequence;

-- name: SessionEventExists :one
-- Whether an event is already stored, for a terminal session: a redelivery of
-- an event stored before the session ended is a duplicate, not a refusal.
SELECT EXISTS (
    SELECT 1 FROM session_events
    WHERE session_id = $1 AND runner_id = $2 AND event_id = $3
);
