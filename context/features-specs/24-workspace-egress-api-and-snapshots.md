Read `CLAUDE.md` before starting.

# M5.4d.2 — Workspace egress API, audit and runner snapshots

**Status: implemented and verified locally; awaiting review. Configuration only — no network access is enabled until M5.4d.3.**
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
  by workspace, authenticated user and operation; fingerprint canonical request fields
  separately (hostname for add, entry ID for remove). Reject reuse with
  different input. Replays recheck current authorization before returning results.
- Invalid/reserved hostname: 400; permission denial: 403; unknown/foreign workspace:
  404; duplicate or cap reached: 409 with distinct stable codes. Declare exact error
  strings/schemas in OpenAPI before handlers. Preserve existing 422 for a key reused
  with a different request and 409/Retry-After for in-flight attempts. Keys remain
  optional, matching the established API. No undocumented fields.
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

### Snapshot creation decision

Use database runner-insert triggers so older runner managers also receive an
explicit snapshot. A BEFORE INSERT trigger validates tenant context equals
NEW.workspace_id and locks the workspace; an AFTER INSERT trigger inserts the
single snapshot row, copying sorted current entries within that same transaction.
The BEFORE lock precedes runner FK checks and AFTER writes, preserving workspace-
first ordering. Duplicate runner insertion rolls back the entire trigger work.
Backfill explicit empty rows for pre-migration runners while additions are empty.

Store `runner_id`, `workspace_id`, `session_id`, `hosts text[]`, `created_at`;
zero hosts is an empty array, never NULL. Enforce one-dimensional, non-null,
unique canonical hosts, cardinality <=20 and tenant-bound runner/session FKs.
No API can edit snapshots. UPDATE/DELETE/TRUNCATE are forbidden except authorized
parent cascades under the existing append-only pattern. The application role
reads snapshots but does not supply their host list. Use a narrowly privileged,
NOLOGIN trigger owner with only required table grants; revoke PUBLIC invocation,
fix search_path, and require the tenant match even in the privileged trigger.
Do not grant clients a general snapshot insert function or tenant bypass.

Old managers still omit added hosts from network policy, which denies rather
than grants access. The additions API is configuration-only in .2; do not release
the complete capability or claim connectivity until all .3 managers/proxies are
installed. The immutable configured list cannot be re-snapshotted on upgrade.
Tests must demonstrate trigger coverage for an unchanged old CreateRunner INSERT,
rollback on failed snapshot creation and preservation on same-runner retry.

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

## Configuration and reserved destinations

Introduce shared `EGRESS_RESERVED_HOSTS`: a comma-separated list of canonical
Weave-operated hostnames/namespaces (each entry also reserves descendants). It
contains no credentials or URLs. In staging/production the API requires a nonempty
explicit deployment declaration; development/test may use an explicit empty list
because internal/local suffixes and built-in destinations are always refused.
Deployment documentation must enumerate the web/API/service names; the declaration
cannot be guessed from bind addresses or DNS. Incomplete deployment input is an
operator configuration error, not a claimed automatic discovery mechanism.

The API constructs a policy from that list plus built-in GitHub/registry hosts and
configured public URLs: `API_BASE_URL`, `NEXT_PUBLIC_APP_URL`, `RUNNER_NATS_URL`
(or NATS_WEBSOCKET_URL), `RUNNER_REGISTRY_PROXY_URL`, and `RUNNER_EGRESS_PROXY_URL`
when present. Share a parser/policy across API and runner manager. Validate any
supplied URL before extracting its hostname; no secret credentials accepted.
Staging/production requires the real public service hosts in the declaration,
including reserved egress-proxy hostname before .3 release. Local suffix rejection
is independent of the declaration. Later provisioning revalidates saved hosts
against current reserved deployment policy; conflict fails closed without
rewriting the immutable snapshot or falling back to direct network rules.

## Replay and lock ordering

Reuse the committed leased Claim/Release mechanism and `completeIdempotency`.
Claim is a short separate transaction, not held across workspace locking. Work
locks workspace, rechecks actor, changes configuration, writes audit, renders the
response and completes the claim with its claimant fence in one transaction.
A stale claimant or render/audit/completion failure rolls everything back.
Releasing a failed claim is fenced and cannot delete a newer claimant's record.

Do not blindly use `idempotency.claim`'s immediate completed-response replay:
recheck membership/permission under the workspace lock before releasing stored
response bytes. DELETE replay sends 204 with no JSON body; existing generic replay
needs a zero-body-aware path. Fingerprinting uses canonical input, not raw bytes.

## Next

Review and merge. Then M5.4d.3: authenticated egress edge using the guarded
transport and runner snapshots, match-free runner forwarding, and the admin UI,
with live forwarding acceptance.

## Implementation record

- Domain: `internal/domain/egress.go` validation and namespace matching.
- Contract: three OpenAPI operations; generated web types.
- Database: migration 00015 and `db/queries/egress.sql`; triggers create an explicit
  immutable snapshot for every runner INSERT, including old-manager syntax.
- Application/store: `internal/application/egress.go`, `egress_config.go`,
  `internal/adapters/postgres/egress.go`. Every read and mutation rechecks current
  `workspace:manage` under the workspace lock; system actors cannot edit policy.
  Mutation, audit and fenced idempotency completion commit together.
- API: `services/api/internal/workspaces/egress.go`. Completed replays and key-reuse
  mismatches recheck current authorization before answering; DELETE replays an
  empty 204. `EGRESS_RESERVED_HOSTS` plus public service URLs are reserved; the
  declaration is required in staging/production.

## Verification record

- Domain, application, HTTP and five PostgreSQL egress tests pass with `-race`.
- Deliberate mutations fail their tests: cap trigger off, snapshot trigger off,
  snapshot read policy broadened, system-actor guard removed, replay
  reauthorization removed, reserved-host check removed, audit write removed.
- Full integration suite: 21 packages pass, none skipped. Run directly because
  `make test-integration` could not rebuild the Docker runner image (Docker Hub
  DNS unreachable); the existing `weave-runner:dev` image was used.
- `make lint`, `typecheck`, `test`, `build`, `sqlc-check`, `contracts-check` pass.
  `make ci` fails only on formatting of pre-existing untracked `.claude/` files.

No provider/live test: this slice changes no runtime network policy.
