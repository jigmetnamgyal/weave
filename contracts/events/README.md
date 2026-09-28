# Event contracts

Versioned session event schemas, plus compatibility fixtures. Filled in M5.3;
see `context/features-specs/16-event-contracts-and-ingestion.md`.

## Layout

- `schemas/envelope.v1.schema.json` — what every event carries.
- `schemas/<type>.v<major>.schema.json` — the payload for one event type at
  one major version. The file name is the transport name without its
  `session.` domain prefix.
- `fixtures/` — examples whose **names state the expected outcome**:
  `*.valid.json` is stored, `*.unknown-field.json` is stored with the unknown
  fields dropped, `*.unknown-major.json` and `unknown-type.json` are
  quarantined. `TestEveryFixtureDecodesAsItsNameSays`
  (`internal/domain/event_test.go`) decodes every file through the
  ingestor's own decoder and refuses a fixture whose name does not say what it
  proves. That test is the drift check between these files and the code.

## Types

| Normalized type   | Transport name               | Emitted from                   |
| ----------------- | ---------------------------- | ------------------------------ |
| `message.created` | `session.message.created.v1` | the provider adapter (M5.5 on) |
| `provider.failed` | `session.provider.failed.v1` | the provider adapter (M5.5 on) |

`plan.updated`, `tool.*`, `file.changed` and the rest arrive with the units
that emit them.

## Transport

JetStream stream `SESSION_EVENTS`, subjects `weave.session.<session_id>.events`
— opaque identifiers only. Work-queue retention: the stream holds what has not
been ingested yet, and `session_events` in PostgreSQL is the permanent
history. Publishers set `Nats-Msg-Id` to the event id.

## Rules

- External transport names use `<domain>.<entity>.<action>.v<version>`.
- Every event carries event ID, schema version, occurred time, producer,
  workspace ID, aggregate ID and correlation ID.
- **Every field is a claim by an untrusted producer.** The session must match
  the subject and the workspace must match the session row, or the event is
  quarantined. `occurred_at` is stored and never used for ordering.
- Consumers are idempotent and tolerate duplicate delivery. Deduplication is
  by `(session_id, producer, event_id)`; ordering is a gapless per-session
  sequence assigned at ingestion.
- Unknown fields are ignored **and not stored**; unknown major versions and
  unknown types are quarantined.
- Payloads stay small — 64 KiB per event. Large data will be an artifact
  reference once object storage exists; until then it is refused.
- The quarantine keeps metadata and a payload hash, never the payload.

See `context/code-standards.md` (Events and Durable Workflows).
