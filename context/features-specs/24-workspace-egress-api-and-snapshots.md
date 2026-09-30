Read `CLAUDE.md` before starting.

# M5.4d.2 — Workspace egress API, audit and runner snapshots

**Status: specification started; no implementation yet.**
M5.4d.1 merged as 43ec4c7 (PR #27). Parent: `22-workspace-egress-allowlist.md`;
security design: ADR-017.

## Outcome

Owners/admins can manage their workspace's additional exact hostnames through a
contracted API. Changes are atomic with audit and idempotency records, cannot
exceed 20 under concurrency, and cannot alter any existing runner's saved list.
This unit does not enable network access: proxy/runner/UI wiring follows in .3.

## Existing patterns to extend

- `internal/application/workspaces.go`: `Actor`, `AuditEvent`, authorization and
  transaction-time membership recheck.
- `db/queries/workspaces.sql`: `LockWorkspace` serializes membership changes;
  reuse the same lock for cap decisions and policy-snapshot serialization.
- `services/api/internal/workspaces`: authenticated workspace resolution and
  existing session idempotency middleware; inspect its transaction/replay behavior
  before reusing it. Do not assume an outer middleware transaction is sufficient.
- `internal/adapters/postgres/runners.go`: runner row creation is a tenant
  transaction. Snapshot persistence must commit with creation, before provisioning.
- Migration 00012: one live runner per session; a duplicate creation must reuse
  that runner's original snapshot, not read a newer workspace policy.
- `internal/adapters/postgres/registry.go`: bounded runner identity resolution,
  followed by tenant-scoped reads. No broad tenant bypass for the future edge.

## API contract

Use existing `/v1/workspaces/{workspaceId}` naming and response conventions.

- `GET /v1/workspaces/{workspaceId}/egress-hosts`: operation `listEgressHosts`;
  response `items` ordered by canonical hostname and `limit: 20`.
- `POST /v1/workspaces/{workspaceId}/egress-hosts`: operation `addEgressHost`;
  input `{hostname}`; response the created entry, HTTP 201.
- `DELETE /v1/workspaces/{workspaceId}/egress-hosts/{egressHostId}`: operation
  `removeEgressHost`; HTTP 204. Foreign/nonexistent IDs have the same 404 response.
- All three require verified membership and `workspace:manage`, as agent profiles
  do. No new permission and no Clerk Organizations.
- Mutation idempotency follows the established session API convention. Scope keys
  by workspace, authenticated user, operation and request hash. Reject reuse with
  different input. Replays recheck current authorization before returning results.
- Invalid/reserved hostname: 400; permission denial: 403; unknown/foreign workspace:
  404; duplicate or cap reached: 409 with distinct stable codes. Declare exact error
  strings/schemas in OpenAPI before handlers. No undocumented fields.
- Entry fields: UUIDv7 `id`, `workspace_id`, canonical `hostname`, `created_by`,
  UTC `created_at`. No secrets or connection data.

## Domain validation

Lowercase ASCII input into one canonical exact hostname; reject whitespace,
trailing dots, Unicode and IDN/punycode labels, wildcards, URLs, ports, paths,
credentials, raw IPv4/IPv6, empty/malformed labels and single-label/internal names.
Bound label length to 63 and hostname length to 253 bytes. Validation performs no
DNS lookup: the later guarded transport validates answers when connecting.

Refuse built-in GitHub/registry hosts and Weave-operated destinations including
the configured ingress, registry proxy, future egress proxy and control-plane
hosts. Before coding the validator, identify every deployment configuration input
needed for this reserved set; an incomplete reserved set is not a safe default.
Do not silently replace a registry forwardURL with an added-host rule.

## Persistence and concurrency

Add a new migration (never edit applied migrations) for `workspace_egress_hosts`
and immutable `runner_egress_snapshots`. Both carry verified workspace scope,
forced RLS, tenant-bound foreign keys and minimal application-role grants.

Workspace entries are mutable configuration (insert/delete only); changes require
one audit event in the same transaction. Stable actions: `workspace.egress_host.added`
and `workspace.egress_host.removed`; record entry ID, canonical hostname and actor.
Audit failure rolls back the configuration change. Replays do not duplicate audit.

Under `LockWorkspace`, recheck the actor's current permission, enforce duplicate
and cap decisions, then write the entry/deletion and audit. Count cannot race with
another addition. Follow existing lock order and inspect idempotency locks for
cycles before implementation. Fail closed on storage errors.

Each runner has one explicit snapshot marker, even for zero additions; absence
must never mean an empty policy. Canonical hosts are immutable, at most 20,
scoped to the runner's actual workspace/session. Allocate the snapshot with the
runner transaction under the same workspace lock as configuration writes.
A new runner allocation reads current entries; retries for an existing runner
read its existing snapshot. Never reconstruct a missing old snapshot from the
current list. Existing runners created before additions can be represented by
an empty snapshot during migration, since no additions previously existed.

Before selecting the exact schema/insert mechanism, verify rolling deployment:
old runner-manager versions must not create ambiguous snapshots. Prefer an
atomic database-enforced creation mechanism if it preserves established tenant
and lock invariants; otherwise stage rollout to prevent old writers before
exposing mutations. Document the decision and exercise it in integration tests.
No API can edit snapshots. UPDATE/DELETE/TRUNCATE are forbidden except authorized
workspace cascades under the existing append-only pattern.

Expose a tenant-scoped snapshot read port for .3. Future edge authorization may
use one bounded privileged runner lookup, then a tenant transaction verifying
live backend/state and exact host membership. Do not create an unrestricted list
endpoint or accept workspace identity from forwarding headers.

## Non-scope

Public egress listener, Vercel policy changes, added-host forwarding, admin UI,
private registries, request-content logging, immediate revocation, or real agent
execution. Do not claim the API alone enables destinations. Deployment of the
complete additions capability waits for .3 acceptance.

## Acceptance and verification

- API authentication/role/foreign-tenant tests for every operation; replay after
  demotion/removal denied; no handler constructs `SystemActor`.
- Domain table tests for normalization and every rejected input class; reserved
  hosts refused and no DNS activity at configuration time.
- PostgreSQL application-role tests prove forced RLS and foreign-key isolation.
  Concurrent adds at the boundary never exceed 20. Audit failure rolls back.
- Identical replay yields one entry/audit; key reuse with other input conflicts;
  remove/re-add gets a new ID without reviving prior snapshots.
- Snapshot races serialize with mutations; empty marker distinct from missing;
  same-runner retry stable; new runner sees updated entries; snapshots immutable.
- Legacy migration/rolling deployment tests and full runner regressions.
- Deliberate mutation tests remove authorization, workspace scoping, cap lock,
  audit transaction and snapshot preservation; regressions must fail.
- `make contracts`, `make sqlc`, `make ci`, `make test-integration` using the
  repository's actual targets. Stop `make dev` workers before integration checks.
  No live provider test is needed to prove this DB/API slice.

## Next

First resolve the reserved-host configuration and snapshot creation/rollout details
against current code. Then contract, migration, domain/store, API and tests, in
that order. After review/merge, .3 integrates the authenticated edge, runner and UI.
