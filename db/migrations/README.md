# Database migrations

Ordered, forward-and-rollback SQL migrations for the Weave control plane.

Empty: no migration exists yet. The first migration arrives with the tenant
model in M2.

## Conventions

- One change per migration, named `<sequence>_<snake_case_description>.sql`.
- Migrations follow **expand / migrate / contract** so that the previous
  application version keeps working during a rolling deploy.
- Never edit a migration that has already been applied to a shared
  environment; add a new one.
- Do not drop a column or tighten a constraint in the same release that stops
  writing the old shape.
- Every tenant-owned table includes `workspace_id` and its indexes.
- Timestamps are `timestamptz`; application-generated primary keys are UUIDv7.

See `context/code-standards.md` (Data and Storage) and
`context/ai-workflow-rules.md` (Data and Migration Rules).
