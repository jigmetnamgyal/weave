# ADR-017: Workspace-added destinations use a guarded egress proxy

- Status: Accepted; implemented in M5.4d.1 (guarded transport), M5.4d.2 (API, audit, snapshots) and M5.4d.3a (edge and runner wiring), verified live on Vercel Hobby. Admin screen: M5.4d.3b.
- Date: 2026-09-30
- Extends: ADR-013 and ADR-016
- Operator approved the proxy approach after the provider-enforcement investigation.

## Context

M5.4d permits admins to add up to 20 exact ASCII hostnames for their workspace's
future runner provisioning. Existing sandboxes keep their original permissions.
An arbitrary allowed hostname can resolve, or later rebind, to a private address.

Vercel's IPv4 subnet deny precedence was verified live. The create API rejected
IPv6 `::/0`; current sandboxes have no usable external IPv6 route, but that is
not a supported enforcement guarantee. A private-address/rebinding fixture would
require infrastructure we do not currently have. Waiting for support or assuming
that a failed connection proves filtering would not establish the boundary.

The operator chose a Weave-enforced destination guard rather than waiting for
provider confirmation. No purchased domain is needed for deterministic tests or
local live forwarding: existing quick tunnels can expose the test proxy.
Production still needs managed TLS endpoints and operational deployment.

## Decision

### A separate edge for additions, not an open registry proxy

Introduce `services/egress-proxy`. Only workspace-added destinations are forwarded
there. ADR-016's registry proxy retains its fixed upstream list and recording
contract. Default GitHub, registry and event-ingress policy is unchanged by this
unit. This ADR does not claim to prove every existing default's private-address
behavior or solve domain fronting on third-party shared infrastructure.

For every added host, the runner manager emits an exact rules-format entry with
`forwardURL = <egress-proxy-base>/e/<host>` and **no match filter**. No addition
may replace a reserved default/ingress/proxy rule. No plain-rule fallback is
permitted if forwarding is unavailable. Staging/production provisioning with
additions fails closed without a valid proxy configuration.

The edge validates Vercel sandbox OIDC just as ADR-016 does: issuer, project,
expiry, signature, and exactly the expected route audience. Forwarded headers
are untrusted claims, checked against the authenticated route. Only HTTPS on
443, supported methods, and an exact encoded path are accepted. The sandbox's
OIDC token and forwarding metadata never reach the origin.

### Authorization is a provisioning snapshot

Persist the canonical additional hosts for each runner before any external
sandbox creation. The snapshot is immutable for that runner and scoped to its
session and workspace. Retrying provisioning for the same runner uses that same
snapshot; allocating a new runner reads the then-current workspace additions.
Snapshot creation must serialize with workspace mutations to avoid a torn list.

Each request must resolve an existing live Vercel runner and find its exact host
in that runner's snapshot. It must not authorize against the mutable workspace
list, take a workspace ID from request headers, or treat a route-bound JWT as
host authorization by itself. An ended runner or database failure is refused.
Use a tenant-scoped store and a narrowly bounded identity lookup analogous to
ADR-016, not a general unscoped reader. No snapshot UPDATE/DELETE by the app role;
workspace retention/cascade behavior follows the existing storage conventions.

This preserves approved effective-time semantics: removing a hostname changes
future snapshots, not existing runner permissions. Users must cancel an existing
session for immediate revocation. The snapshot is not a second editable policy.

### Resolve, validate, then dial the validated address

For every **new upstream connection**:

1. Resolve the authorized hostname with a bounded, cancellable resolver.
2. Validate all returned addresses. Reject the entire result if empty or if any
   answer is forbidden; never silently pick a public answer from a mixed set.
3. Permit only explicitly supported public unicast addresses. Reject private,
   loopback, unspecified, link-local, multicast, metadata, reserved/special-use,
   and IPv4/IPv6 transition or translation ranges that could reach forbidden
   destinations. Normalize IPv4-mapped IPv6 before validation. `IsGlobalUnicast`
   alone is insufficient. Maintain an explicit reviewed range table and tests.
4. Connect using a numeric validated address, not the hostname. Every fallback
   candidate comes from that same validated set: **no second DNS resolution**.
5. Keep the original authorized hostname for HTTP Host, TLS SNI, and certificate
   verification; never use insecure TLS or rewrite Host to the numeric address.

Do not use environment-configured HTTP proxies, an unrestricted alternate
transport, cross-origin connection pooling, automatic redirects, upgrades,
CONNECT tunneling or WebSocket tunnels. HTTP/HTTPS requests only. Existing
connections may be reused for their validated origin; the guard runs again when
opening a new connection. Disabling reuse alone is not a rebinding defense.

No secret is deliberately injected by this feature. Ordinary origin headers may
contain credentials supplied by the caller, so this edge joins the trusted
plaintext boundary already occupied by Vercel's forwarding firewall. Preserve
only origin-relevant headers; strip internal identity/forwarding and hop-by-hop
headers in both directions. Do not log headers, bodies, paths, queries or origin
response contents. Added-host request-content logging is not introduced. Changes
to the allowlist remain audited with actor and canonical hostname.

### Fail closed and bound resources

Bound DNS, dialing, TLS negotiation, response headers, body reads, request body
size and concurrency. Do not introduce indefinite body draining or unbounded
buffering. Responses may stream without a fixed total write timeout, but obey
cancellation and bounded service capacity. Do not forward a refused or oversized
body prefix. Errors expose stable safe categories, not raw DNS answers or URLs.
Keep readiness separate from the public forwarding listener.

## Verification before release

- Inject resolver and numeric-dial seams into a guarded transport, not into
  production bypass flags. Deterministic tests control public, private, mixed,
  mapped, transition, empty, timeout and changing DNS answers.
- Observe the actual dial target. A resolver returning a public answer then a
  private answer must not induce a second lookup before dialing; opening a later
  connection must reject the new forbidden answer before any dial.
- Use a local test fixture behind test-only dial/resolver seams. Never add an
  allow-private environment flag or require real private-network infrastructure.
  Preserve production classification and assert TLS hostname verification.
- Mutation checks remove validation, restore hostname dialing, enable redirects
  or remove snapshot authorization; each must fail its boundary regression.
- Opt-in Vercel forwarding tests exercise the real edge/token/firewall route and
  demonstrate success against a benign public origin. They complement, not
  replace, deterministic resolver/transport tests. No paid services in CI.
- Tenant/RLS, snapshot serialization, cap concurrency, idempotency, audit rollback,
  ended-runner, malformed-header, timeout and body-limit tests remain mandatory.

## Alternatives

- **Wait for Vercel support / buy private test infrastructure:** deferred as a
  prerequisite for additions. Existing experiments remain useful evidence, not
  discarded or restated as full proof.
- **Check DNS only at host creation:** rejected; DNS can change afterwards.
- **Resolve then use the default hostname dialer:** rejected; the second lookup
  reintroduces the time-of-check/time-of-use vulnerability.
- **Broaden the registry proxy:** rejected; it weakens a fixed upstream boundary
  and conflates registry recording with arbitrary workspace-approved traffic.
- **Disable additions or permit plain direct rules:** not the approved outcome.

## Consequences and delivery

We own another security-sensitive internet-fetching edge and its availability,
resource limits, plaintext handling and deployment. An unavailable edge prevents
added-host traffic. It must not inherit the control plane's private-network access.
Deny Weave-operated/reserved destinations at host validation as defense in depth.

Split M5.4d into bounded implementation units:

1. **M5.4d.1:** guarded resolver/dial transport and deterministic security tests.
2. **M5.4d.2:** contracted additions API/store/audit and immutable runner snapshots.
3. **M5.4d.3:** authenticated edge, runner wiring and admin UI; end-to-end acceptance.

All are required before declaring additions complete. Provider deployment/staging
requirements in ADR-013 remain unchanged. No runtime behavior changes in this ADR.

## Implementation notes (M5.4d.3a)

- Live verification needs a private-address host whose traffic leaves the
  sandbox: a loopback name (tried first with `localtest.me`) is dialed by the
  sandbox itself and never reaches the firewall. `10.0.0.1.nip.io` does reach
  the proxy and is refused by the guard with `ErrDestination`.
- The development quick tunnel replaces an origin's 502 body with its own error
  page, so the sandbox cannot see why a request failed; the live test records
  the guarded transport's outcome directly instead.
- The proxy reuses the registry proxy's bounded runner lookup
  (`weave_runner_for_registry_request`): one id in, at most one row out.
