Read `CLAUDE.md` before starting.

# M5.4d — Workspace egress allowlist additions

**Status: ADR-017 accepted; M5.4d.1 transport built, awaiting review; API/snapshots/edge/UI pending.**

## Outcome

Workspace owners and admins can authorize additional exact HTTPS destinations for
that workspace's runners, see the current additions, and remove them. Every
change is attributable and audited. M5.4c remains intact: no addition can replace
a default registry's forwarding rule or broaden the registry proxy's upstreams.

## Enforcement approach — ADR-017

Workspace-added hosts are forwarded, without match filters, to a separate
`services/egress-proxy`. It authenticates Vercel OIDC, authorizes the exact host
against an immutable runner provisioning snapshot, and resolves/validates/dials
only public addresses with TLS hostname verification. No redirect following,
environment proxy, hostname redial or plain-rule fallback is allowed. The
registry proxy remains fixed-list and unchanged. Deterministic injected resolver
and dial tests replace the need for a purchased domain/private-network fixture;
real Vercel forwarding remains an opt-in end-to-end check.

See `docs/adr/0017-workspace-egress-proxy.md` for the threat model, plaintext
handling, resource bounds and snapshot semantics. Delivery is split into
M5.4d.1 guarded transport, M5.4d.2 additions API/audit/snapshots, and M5.4d.3
authenticated edge/runner/UI. None is independently the complete additions feature.

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
6. The separate authenticated proxy and guarded transport described in ADR-017.
7. A workspace Security settings surface: list, add form, removal confirmation,
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
   new runner allocations. A retry for the same runner retains its immutable
   snapshot. Existing sandboxes retain their policy until
   teardown. Removing a hostname does not immediately revoke active connections;
   the UI must say this and offer the existing session cancellation path.
3. **Canonicalization:** lowercase ASCII hostnames, no trailing dot, no
   Unicode/IDN input, no wildcard; reject reserved/internal names. Document the
   exact reserved list from deployment configuration before implementation.
## Security gate — revised by the operator-approved proxy approach

Provider private-address/rebinding guarantees are no longer the prerequisite
for workspace-added hosts. ADR-017 requires our guarded transport to enforce
address validation and numeric dialing on every new upstream connection,
including IPv6. The prior provider experiments remain observations, not guarantees.
The implementation gate is now deterministic security verification, snapshot
authorization and real forwarding acceptance. Do not claim it closed before tests.

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
- Guarded-transport tests: changing DNS answers, forbidden/mixed address sets,
  numeric-only dialing, TLS verification, redirects and canceled/timed-out reads.
- Opt-in Vercel acceptance: forward one benign public host, deny its
  subdomain, verify token/snapshot authorization, and re-run the registry
  forwarding matrix. Never run paid/live checks in CI. Stop `make dev` workers
  before integration/live session checks; tear down all sandboxes and snapshots.
- Repository gates: `make ci`, `make test-integration`, `make test-vercel-live`
  when explicitly configured. Read installed Next.js guides before writing UI code.

## Delivery status

Partial implementation only. Guarded transport built in M5.4d.1 (`23-guarded-egress-transport.md`); not wired
to runtime. No migration, API contract, public edge, policy changes or UI yet.
Staging still requires Vercel Pro, the nine-hour survival test and data-processing
terms; Hobby results do not close those gates.

## Provider enforcement spike — partial result

Source: https://vercel.com/docs/sandbox/concepts/firewall (checked during M5.4d).
Vercel documents `subnets.deny` and says denied ranges override domain rules.
The opt-in `TestLiveDeniedRangesOverrideAllowedHosts` confirms this for IPv4:
GitHub returns its real server header under an exact hostname rule; the same
rule with `0.0.0.0/0` denied cannot reach it. Removing the deny makes the test
fail. The test deliberately forces IPv4; it makes no IPv6 claim.

The create API rejected `::/0` with HTTP 400, `Invalid CIDR "::/0"`.
Do not ship a deny list that assumes IPv6 CIDRs are accepted. Establish the
provider's IPv6 connectivity and enforcement contract before choosing the policy.
Production policy is unchanged: this is a live-only enforcement experiment.

Still needed: a reachable controlled fixture at a forbidden address and a
controlled authoritative DNS name that changes from public to forbidden answers.
A connection failure against an address with no server would not prove filtering;
likewise, curl's `--resolve` does not change the firewall's DNS resolution.
That provider-dependent path is superseded by ADR-017 for added hosts; these
observations are retained as history. The guarded-proxy implementation gate
remains open. No feature implementation yet.

### IPv6 connectivity snapshot

`TestLiveIPv6ConnectivitySnapshot` observes the current sandbox: IPv6 is enabled
in the kernel, with loopback and link-local addresses but no usable external
IPv6 route. Public AAAA records resolve. The IPv4 Cloudflare control receives
its real server response; forcing IPv6 fails with curl exit 7. This is not a
contractual IPv6 security guarantee. Only the selected server header is emitted,
not response cookies. Fixture prerequisites and provider questions are documented
in `docs/runbooks/workspace-egress-verification.md`.
