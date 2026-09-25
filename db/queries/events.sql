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
