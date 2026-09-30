Read `CLAUDE.md` before starting.

# M5.4d.3 — Egress proxy, runner wiring and admin UI

**Status: 3a built and verified live on Vercel Hobby, awaiting review; 3b pending.** M5.4d.2 merged as 6a1193a
(PR #28). Parent: `22-workspace-egress-allowlist.md`; design: ADR-017.

This unit makes a workspace's added hostnames reachable from its runners, and
gives admins a screen to manage them. It is split, as M5.4a–c were, because the
two halves share no code and are independently reversible:

- **M5.4d.3a — egress proxy and runner wiring** (runner plane + public edge).
- **M5.4d.3b — admin settings screen** (web only, over the merged API).

M5.4d is complete only when both have merged and 3a's live acceptance passes.

## Existing pieces this connects

- `internal/adapters/guardedhttp`: resolve, validate every answer, dial numeric
  public addresses, verify original-host TLS, no redirects (M5.4d.1).
- `runner_egress_snapshots` and `EgressStore.Snapshot`: immutable per-runner host
  list, explicit empty row, `ErrEgressSnapshotMissing` when absent (M5.4d.2).
- `services/registry-proxy`: the pattern for a public edge authenticated by
  Vercel sandbox OIDC — route-bound audience, forwarded-header checks, bounded
  body read, bounded runner lookup, no redirect following (ADR-016).
- `internal/application/runners.go` `egressRules`: builds each runner's policy.
- `internal/adapters/vercelsandbox/policy.go` `validForwardURL`: today accepts only
  the registry route `/r/<host>`.

## M5.4d.3a — egress proxy and runner wiring

### Outcome

A runner can reach exactly the hosts in its own snapshot, over HTTPS, through a
Weave proxy that refuses forbidden addresses on every new connection. A host the
runner's snapshot lacks, a subdomain of an added host, or a host that resolves to
a private address, cannot be reached.

### New service: `services/egress-proxy`

Mirrors the registry proxy's shape: public and health listeners, the same config
conventions, readiness on PostgreSQL and Vercel's signing keys.

Per request, in order, refusing at the first failure:

1. **Route** `/e/<host>/<path>`, compared in encoded form (the lesson of PR #26).
2. **Authenticate** `Vercel-Sandbox-Oidc-Token`: signature, issuer, project, expiry,
   and audience exactly `<EGRESS_PROXY_PUBLIC_URL>/e/<host>`. Reuse the existing
   verifier. No token or a bad one: 401 before any database or network access.
3. **Check forwarded headers** against the route: host equal, scheme `https`,
   port absent or 443, path equal to the request path. Mismatch: 400.
4. **Resolve the runner** from `sandbox_name` through the existing bounded lookup
   (`weave_runner_for_registry_request`: one id in, at most one row). It must be a
   live runner of the Vercel backend. Otherwise: 403.
5. **Authorize the host** in a tenant transaction scoped to that runner's own
   workspace: read its snapshot, require the exact host. Missing snapshot, host not
   in it, or any database error: 403 / fail closed. Never consult the mutable
   workspace list, and never take a workspace from a header.
6. **Read the body**, bounded (8 MiB, 60-second deadline cleared only on success),
   before anything is sent. Oversized: 413.
7. **Forward** through `guardedhttp.Client` to `https://<host><path>`. Strip Vercel
   identity/forwarding headers and hop-by-hop headers both ways. Redirects return
   to the sandbox unfollowed, so its own policy decides the next hop. Refused
   destination: 502 with a stable safe category, never the resolved address.

No request-content logging: log only method, host, outcome category, runner id
and latency. Never the path, query, headers or body. The service holds no
provider or GitHub credential, and gets no private-network reachability beyond
the database.

### Runner wiring

- New runner-manager setting `RUNNER_EGRESS_PROXY_URL`: https, no path (shared
  validation with the registry proxy URL). **Required in staging and production.**
  In development it may be unset.
- At provisioning, before calling the backend, read the runner's snapshot in its
  tenant. A missing snapshot fails provisioning. A non-empty snapshot with no
  egress proxy configured fails provisioning. **Never** a plain rule for an
  added host, in any environment.
- Revalidate each snapshot host against the current reserved policy (built-ins,
  `EGRESS_RESERVED_HOSTS`, configured service URLs). A host now reserved fails
  provisioning. Never rewrite the snapshot; the snapshot stays immutable.
- Each added host becomes `EgressRule{Host, ForwardURL: <base>/e/<host>}`, with no
  `match`. Added hosts cannot collide with default rules, which are reserved.
- Generalize `validForwardURL` to accept exactly `/r/<host>` or `/e/<host>`, host
  equal to the rule's.
- The dev Docker backend still enforces nothing and ignores forwarding.

### Failure behavior

Proxy unavailable: added-host requests fail at the sandbox; defaults are
unaffected. Database unavailable: every request refused. A session whose runner
cannot be provisioned fails through the existing provisioning failure path, with
a reason naming the egress configuration and never the hostnames' resolution.

### Acceptance

- Unit/handler tests for every refusal in the request order above, including
  a valid token for a runner whose snapshot lacks the host, an ended runner, a
  missing snapshot, a snapshot in another tenant, oversized/stalled bodies,
  redirect passthrough and header stripping.
- The forward path uses `guardedhttp` (a forbidden resolved address is refused
  through the real handler, via the client's test seams).
- Runner tests: snapshot read; fail-closed on missing snapshot, missing proxy URL
  and now-reserved host; rules forwarded with no match; retries reuse the snapshot.
- Policy tests: `/e/<host>` accepted only for its own host.
- Deliberate mutations: drop the snapshot check, drop the audience route binding,
  use a plain rule for an added host, bypass `guardedhttp`. Each must fail a test.
- **Live, opt-in (`vercel_live`), with a third tunnel for the egress proxy:** a
  sandbox whose snapshot holds one benign public host reaches it through the
  proxy; its subdomain and an unlisted host are refused; the registry matrix
  still passes. Sandboxes and snapshots gone afterwards.

## M5.4d.3b — admin settings screen

Read the installed Next.js 16 guides under `node_modules/next/dist/docs/` first.

- A page under the workspace (for example `settings/egress`), linked from the
  workspace navigation, visible to owners and admins; others see why they cannot
  manage it rather than an empty list.
- List with hostname, who added it and when, count against the limit of 20.
- Add form with inline validation errors from the API; removal with confirmation.
- States: loading, empty, error distinct from empty, permission denied, at-limit.
  Preserve typed input on a recoverable error.
- Plain warnings, as the spec requires: an added host can receive repository
  data; changes apply to **new** sessions only, and removing a host does not cut
  off a session already running — cancel it for that.
- Idempotency keys on add and remove, stable across a retry of one submission.
- Server actions over the generated API types, following the existing members
  page and actions. Semantic tokens only; keyboard accessible.

## Non-scope

Wildcards, ports, private registries, credentials for added hosts, request-content
logging for added hosts, immediate revocation of running sessions, and any change
to default GitHub/registry/ingress rules.

## 3a implementation record

- `internal/application/egress_forward.go`: `EgressForwardURL`, `EgressAuthorizer`
  (bounded runner lookup, then the snapshot in the runner's own tenant, exact
  host), and fail-closed `addedEgressRules`.
- `internal/application/runners.go`: `WithEgress`; provisioning reads the runner's
  snapshot before minting credentials or calling the backend, and refuses when
  it is missing, when added hosts have no proxy, when a host is not canonical or
  is now reserved, and when snapshots were never wired.
- `internal/adapters/vercelsandbox/policy.go`: a forward URL must be exactly
  `/r/<host>` or `/e/<host>` for its own host, at the proxy's root.
- `services/egress-proxy`: config, handler and entry point; ports 8097/8098.
  Defaults to `guardedhttp.New()`. `guardedhttp.NewWithResolver` added for tests:
  resolver injection only, no dialer or TLS seam.
- Runner manager: `RUNNER_EGRESS_PROXY_URL` (required in staging/production) and
  `EGRESS_RESERVED_HOSTS` via the shared reserved-config parser.
- `scripts/dev-tunnel.sh` starts a third tunnel; `make test-vercel-live` runs the
  egress proxy's live test.

## 3a verification record

- Handler tests: every refusal in request order (route, raw-IP route, token
  missing/for another host/replayed on another authorized route/for the registry
  route, non-runner sandbox, forwarded host/scheme/port/path mismatch, host not in
  snapshot, oversized and stalled bodies, authorization failures), clean headers
  both ways, redirect passthrough, and private answers refused through the real
  guard with no address in the body.
- Application tests: forwarded rules; five fail-closed provisioning cases with
  no backend call; the authorizer's eight refusal cases, read in the runner's own
  tenant. PostgreSQL: authorization through the real lookup and snapshot, where
  later removal does not revoke and later addition does not grant.
- Deliberate mutations fail: snapshot check dropped, audience fixed to one host,
  added host given a plain rule, default upstream not the guarded transport.
- Live on Vercel Hobby: an added host returned its real page through the proxy;
  a forwarded host missing from the snapshot got 403 and never reached upstream;
  `10.0.0.1.nip.io` was authorized, then refused by the guard (`ErrDestination`);
  an unlisted host and a subdomain did not resolve. The registry and backend live
  suites still pass. Afterwards 0 sandboxes, 0 snapshots; no token in any log.
- `make test-integration` (22 packages), lint, typecheck, test, build, sqlc and
  contracts checks pass.

## Next

Review and merge 3a. Then 3b, the admin screen. Staging gates from ADR-013 still apply.
