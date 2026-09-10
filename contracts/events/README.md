# Event contracts

Versioned runner and session event schemas, plus compatibility fixtures.

Placeholder: no schemas are defined yet. The first schemas arrive with the
session workflow in M5.

## Rules

- External transport names use `<domain>.<entity>.<action>.v<version>`.
- Every event carries event ID, schema version, occurred time, producer,
  workspace ID, aggregate ID and correlation ID.
- Consumers are idempotent and tolerate duplicate delivery.
- Unknown fields are ignored; unknown major versions are rejected and
  quarantined.
- Payloads stay small — large data is stored as an artifact reference.

See `context/code-standards.md` (Events and Durable Workflows).
