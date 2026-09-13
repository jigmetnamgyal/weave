# OpenAPI contracts

Canonical description of the Weave control-plane HTTP API.

Covers the `/v1` surface: identity, workspaces, membership, invitations, and
GitHub installations. The operational probes `GET /health/live` and
`GET /health/ready` are deliberately absent — they are not part of the
versioned public API.

**Known gap.** The second rule below is not yet satisfied: no client is
generated from this file, and `apps/web/lib/api.ts` still hand-writes request
helpers and response types. That predates the GitHub endpoints rather than
being introduced by them, and closing it means choosing a generator, wiring a
drift check into CI, and migrating every existing call site — its own unit of
work, not a footnote to a feature.

## Rules

- The contract is updated **before or with** the implementation it describes.
- The TypeScript client is generated from this source; it is never hand-written
  or duplicated in `apps/web`.
- Externally consumed APIs are versioned. Breaking changes require a new
  version and a migration plan.
- Additive, compatible fields must not break existing consumers.

See `context/code-standards.md` (API Routes) and `context/architecture.md`.
