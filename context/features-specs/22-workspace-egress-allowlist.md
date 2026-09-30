Read `CLAUDE.md` before starting.

# M5.4d — Workspace egress allowlist additions

**Status: policy rules approved by the operator; provider security verification pending before implementation.**

## Outcome

Workspace owners and admins can authorize additional exact HTTPS destinations for
that workspace's runners, see the current additions, and remove them. Every
change is attributable and audited. M5.4c remains intact: no addition can replace
a default registry's forwarding rule or broaden the registry proxy's upstreams.

## Sources and existing boundary

- ADR-013, section 3: self-serve additions under `workspace:manage`, audited,
  capped, hostname-only, tenant-scoped; private/internal destinations always denied.
- ADR-015: the public event ingress is the sole approved Weave-operated exception.
- ADR-016 and `21-registry-request-logging.md`: all default registry requests
  forward without a match condition and are recorded before fetching.
- `internal/application/runners.go`: the runner service currently constructs
  default egress rules and the ingress rule before provisioning.
- `internal/application/registry.go`: default registry hosts and forwarding routes.

Clerk authenticates users only. Membership, workspace scope and permissions remain
in PostgreSQL, per ADR-009; do not introduce Clerk Organizations.

## Scope

1. Domain validation and a tenant-scoped additions store.
2. An additive migration for `workspace_egress_hosts`, forced RLS, unique
   `(workspace_id, hostname)`, opaque UUIDv7 IDs, UTC creation timestamp and creator.
3. OpenAPI-contracted list, add and remove operations under
   `/workspaces/{workspace_id}/egress-hosts` (final route spelling follows existing
   contract conventions). Mutations use the existing idempotency mechanism.
4. Authorization on every operation. Proposed: `workspace:manage` for both reads
   and writes; mutations and audit rows commit in the same transaction.
5. A bounded read at runner provisioning to compose additions with existing rules,
   without modifying registry forwarding or trusting a client-supplied policy.
6. A workspace Security settings surface: list, add form, removal confirmation,
   cap display, permission-denied, empty, loading, offline and retry states.

Implementation should first contract and verify the store/API boundary, then
integrate runner policy and UI. Any split preserves this spec's acceptance gates.

## Non-scope

Wildcards, URLs, paths, ports, private registries, credential injection, hostname
ownership verification, arbitrary proxy forwarding, request logging for added
hosts, provider-specific policy changes, and hot edits to running sandboxes.
No promise of Docker egress enforcement: the dev backend remains unenforced and
refused outside development. No real agent execution before M6.

## Security and validation

- Resolve a verified workspace membership before any tenant access; foreign IDs
  must not reveal another workspace's entries or audit data.
- Accept exact hostnames only. Define canonicalization once, then validate and
  persist that canonical value. Reject IP literals, wildcards, credentials,
  schemes, ports, paths, empty labels and malformed DNS names. Reject Unicode/IDN input; never silently broaden a host to its subdomains.
- Reserved defaults, ingress, proxy and other Weave-operated destinations cannot
  be added as plain rules. A duplicate cannot overwrite a registry forwardURL.
- Metadata, private, loopback and link-local addresses remain unreachable even
  through public names, CNAMEs, changed DNS answers or DNS rebinding. A one-time
  application DNS check is not sufficient enforcement.
- Atomically enforce the cap under concurrent additions, using a workspace-level
  lock or equivalent serialization; uniqueness alone does not enforce a cap.
- Store only the canonical hostname and attributable change metadata. No secrets
  or connection headers enter audit rows or logs.
- Show an explicit warning: an added destination can receive repository data.
  Admin approval is a deliberate expansion of the sandbox's trust boundary.

## Approved policy rules

Approved by the operator after explanation of the tradeoffs:

1. **Cap:** 20 additional hostnames per workspace (defaults excluded).
2. **Effective time:** changes affect future provisioning only, including
   retries that create a new sandbox. Existing sandboxes retain their policy until
   teardown. Removing a hostname does not immediately revoke active connections;
   the UI must say this and offer the existing session cancellation path.
3. **Canonicalization:** lowercase ASCII hostnames, no trailing dot, no
   Unicode/IDN input, no wildcard; reject reserved/internal names. Document the
   exact reserved list from deployment configuration before implementation.
## Remaining security gate

**Private-destination guarantee:** demonstrate Vercel's firewall blocks public
   names resolving to forbidden ranges, including rebinding. If it does not,
   stop and select an enforcing design in an ADR; do not ship a DNS-check-only
   workaround or claim compliance with ADR-013.

## Failure behavior

Invalid/reserved hostnames are validation failures; cap and duplicate failures
have stable contract error codes. Unauthorized mutations change neither entries
nor audit rows. Audit failures roll back the mutation. Idempotent retries do not
duplicate entries or audit events. Failure to read additions during provisioning
fails closed rather than provisioning an incomplete policy. A provider refusal
fails provisioning explicitly and uses existing cleanup and retry behavior.
UI errors preserve entered text and explain that the previous policy is intact.

## Acceptance

- Authorized admin adds a hostname, observes it, and removes it; each mutation has
  exactly one audit event and replaying the request creates no duplicate side effect.
- Developers, viewers, unauthenticated users and foreign tenants cannot mutate.
- Concurrent additions cannot exceed the accepted cap.
- New runners receive only their workspace's additions; default registries retain
  their exact forwarding routes and match-free rules.
- Removal semantics match the approved effective-time decision and UI wording.
- Invalid hosts, reserved services, raw IPs and forbidden resolved addresses are
  refused. A deliberately rebinding name cannot defeat the private-range boundary.
- Added-host subdomains are not implicitly allowed. Registry recording still works.
- The UI passes keyboard navigation and shows failure distinct from an empty list.

## Verification plan

- Domain table tests for canonicalization, reserved hosts and boundary lengths.
- API tests for authentication, roles, cross-tenant IDs, error contracts and replay.
- PostgreSQL integration tests for forced RLS, atomic audit rollback, uniqueness,
  idempotency and concurrent cap enforcement.
- Runner tests for tenant-scoped composition, registry-rule precedence and failed
  reads. Mutation checks remove each security guard and demonstrate test failures.
- Opt-in Vercel acceptance: allow one controlled public test host, deny its
  subdomain, test forbidden DNS answers and rebinding, and re-run the registry
  forwarding matrix. Never run paid/live checks in CI. Stop `make dev` workers
  before integration/live session checks; tear down all sandboxes and snapshots.
- Repository gates: `make ci`, `make test-integration`, `make test-vercel-live`
  when explicitly configured. Read installed Next.js guides before writing UI code.

## Delivery status

Spec only. No migration, API contract, policy changes or UI implementation yet.
Staging still requires Vercel Pro, the nine-hour survival test and data-processing
terms; Hobby results do not close those gates.
