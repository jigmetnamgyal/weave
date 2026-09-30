Read `CLAUDE.md` before starting

We're adding ADR-013's control on the registry side channel (Unit M5.4c): **every
package-registry request a runner makes is recorded, per session, with its
package path**, so a session fetching packages no manifest in its repository
names is visible — and, later, alertable. It is required before M6, the first
unit in which a real agent runs in a customer's repository.

Read `docs/adr/0013-runner-execution-boundary.md` (the control, and why
registries are a channel at all), `docs/adr/0015-runner-event-ingress.md` (the
pattern for a public edge), `context/features-specs/20-vercel-runner-backend-and-event-ingress.md`
(the backend this extends) and ADR-016, written with this unit.

## M5.4c is split before building

The tracker's M5.4c was "registry request logging and workspace allowlist
additions". They share nothing but a firewall:

- **Registry request logging** is runner-plane and edge work: a new public
  service, the sandbox's network policy, and a table.
- **Workspace allowlist additions** are a control-plane slice: an API, a table,
  an audit trail, a cap and a UI, authorized by `workspace:manage`.

The project rules split a step that combines unrelated boundaries, and M5.4a,
M5.4b and M5.5 were each split before building for the same reason. So:

- **M5.4c (this unit)** — registry request logging.
- **M5.4d** — workspace allowlist additions. Also before M6.

## The mechanism, measured before it was designed

ADR-013 names Vercel's `forwardURL` rule and nothing more. A spike on
2026-09-30 (Hobby, throwaway code, two sandboxes, both deleted) measured what it
does, because the reference does not say:

- **Policy shape.** `forwardURL` exists only in the rules format of the network
  policy — `{"allow": {"<host>": [<rule>, …]}}` — not in the
  `mode`/`allowedDomains` format M5.4b uses. A host with an empty rule list is
  plainly allowed. Only one of `transform`, `forwardURL` or `response` may be set
  per rule.
- **What the proxy receives.** Vercel's firewall terminates the sandbox's TLS
  and sends an ordinary **origin-form HTTP/1.1 request to the `forwardURL`, with
  the original path appended** — `forwardURL` `…/fwd/X` and a request for
  `/left-pad` arrive as `/fwd/X/left-pad`. The real destination is in headers:
  `Vercel-Forwarded-Host`, `Vercel-Forwarded-Path` (with the query),
  `Vercel-Forwarded-Scheme`, `Vercel-Forwarded-Port`. Method and body pass
  through (a `POST` arrived with its body).
- **The response flows back.** npm, inside the sandbox, received the spike
  proxy's answer. Tools trust the interception through CA-bundle variables the
  image already sets (`NODE_EXTRA_CA_CERTS`, `PIP_CERT`, `SSL_CERT_FILE`,
  `CARGO_HTTP_CAINFO`, …).
- **Authentication comes free.** Every forwarded request carries
  `Vercel-Sandbox-Oidc-Token`, an RS256 JWT from
  `https://oidc.vercel.com/<team>`, whose keys are published at the issuer's
  `/.well-known/jwks`. Its claims: `iss`, `project_id`, `sandbox_id`,
  **`sandbox_name`** — which for Weave's sandboxes contains the runner id —
  and **`aud` equal to the exact `forwardURL`**, path included. It expires in 24
  hours. The sandbox never sees it: the firewall adds it on the way out.
- **Deny-by-default holds in the rules format**: an unlisted name does not
  resolve, as in M5.4b's verified format.
- **The trap.** A request to a host with rules that **no rule matches is sent
  straight to the origin, unlogged** — pypi's `/project/…` went direct while the
  rule matched only `/simple/`. So a logging rule must never carry `match`.

## What this unit builds

### The runner's network policy, in the rules format

`RunnerSpec.EgressHosts []string` becomes `RunnerSpec.Egress []EgressRule`,
each `{Host, ForwardURL}`, built by the runner manager, never from input:

- `github.com`, `codeload.github.com` and the event ingress — plain, as today.
- **Every registry host on ADR-013's default list — forwarded** to
  `<RUNNER_REGISTRY_PROXY_URL>/r/<host>`, with **no `match`**, so every request
  to that host is forwarded and none can slip past to the origin.

The Vercel adapter emits the rules format, with the same validation as M5.4b
(exact lowercase hostnames, no wildcards, no IPs) plus: a `forwardURL` must be
`https`, with no user information, query or fragment (Vercel's own rule). No
hosts is still `deny-all`. The dev backend enforces nothing and ignores
forwarding, as it ignores the allowlist; it is refused outside development.

**`RUNNER_REGISTRY_PROXY_URL`** is the proxy's public base URL. **In staging and
production the runner manager refuses to start without it**: an unset value
would let registry traffic through unrecorded, the exact gap this unit closes.
In development it may be unset, and registries are then plainly allowed with a
startup warning saying so.

### The registry proxy: `services/registry-proxy`

A new deployment unit — the second public edge, after ADR-015's event ingress.
It does one thing: **authenticate, record, forward**.

1. **Authenticate** the request's `Vercel-Sandbox-Oidc-Token` with
   `lestrrat-go/jwx` (already used for Clerk; no custom cryptography):
   signature against the configured team issuer's JWKS, `exp`/`nbf` with a
   small leeway, `iss` equal to the configured issuer, `project_id` equal to the
   configured runner project, and **`aud` equal to this proxy's own public base
   plus the request's `/r/<host>` prefix** — so a token issued for one route
   cannot be replayed on another. No token, or one that fails any check: `401`,
   nothing forwarded, nothing recorded as a registry request.
2. **Resolve the runner** from `sandbox_name`, which the Vercel backend derives
   from the runner id (`weave-runner-<scope>-<uuid>`). The runner must exist, be
   a Vercel runner, and be live (provisioning, running or terminating). Looked
   up through a bounded `SECURITY DEFINER` function, the pattern the ingestor
   and the reconciler use, returning only the runner's session, workspace,
   backend and state. Unknown or ended runner: `403`.
3. **Check the destination.** `Vercel-Forwarded-Host` must equal the `<host>` in
   the path, and be on the proxy's registry list; scheme `https`, port `443`;
   `Vercel-Forwarded-Path` must begin with `/`. Anything else: `400`. **The
   proxy is not an open proxy**: it fetches only from its fixed registry hosts.
4. **Record, then forward.** One append-only row in `registry_requests` —
   workspace, session, runner, host, method, path **without its query**, and
   the time — written in the tenant context of the runner's workspace **before**
   anything is fetched. **A request that cannot be recorded is not forwarded**
   (`503`): the control fails closed, never open.
5. **Forward** to `https://<host><path>` with a real TLS check: method, body and
   end-to-end headers pass through; hop-by-hop headers and everything the path
   added (`Vercel-*`, `Cf-*`, `Cdn-Loop`, `X-Forwarded-*`) are dropped; redirects
   are **returned to the sandbox, never followed** here — the sandbox's own
   policy then decides whether their target is reachable. The response streams
   back with its status and end-to-end headers.

Readiness is PostgreSQL and the JWKS having been fetched at least once. The
proxy holds no credential of its own beyond its database role.

### The table

`registry_requests`: `id` (UUIDv7), `workspace_id`, `session_id`, `runner_id`,
`host`, `method`, `path`, `requested_at`. Row-level security in the creating
migration, forced; append-only by trigger (no UPDATE, DELETE, TRUNCATE, except
the workspace cascade, as `session_events`); an index on `(session_id,
requested_at)`. `path` is capped (2,048 characters, cut on a rune boundary) and
`method` checked against a closed set. **No query string, header or body is
ever stored** — a query can carry a token, and the path is the package.

## Non-scope

- **Detection**: comparing a session's fetched packages with the manifests in
  its repository, and alerting — the next step once the record exists; the
  record is what this unit guarantees.
- Workspace allowlist additions — M5.4d.
- Private registries and their credentials. They would pass through a proxy
  and a firewall that terminate TLS; M5.4d and M6 decide whether to allow them.
- A package cache or pull-through mirror (ADR-013's revisit).
- Logging on the dev backend, which enforces no egress at all.
- A read API or UI for the record — the session room (M6) is where people read
  history.

## Security

- **A second public endpoint.** It authenticates every request with a token the
  sandbox never holds, signed by Vercel for our team and project and bound to
  the exact route. Pre-authentication exposure is TLS and an HTTP parser;
  unauthenticated requests are refused before any database or upstream work.
  Hosting follows ADR-015's rules: TLS at Weave's edge in production, a
  per-source limit in front, a tunnel only in development.
- **Vercel sees registry traffic in plaintext.** Its firewall terminates TLS to
  forward. For the default public registries that is public content; it is a
  reason the spec defers private registries.
- **The proxy is an egress point of Weave's own infrastructure**, restricted to
  the fixed registry hosts, HTTPS only, following no redirects.
- **Threat-model change** recorded in ADR-013 and ADR-016, as the
  protected-files rule requires for an egress-policy change.

## Failure behaviour

- **Proxy down or unreachable:** registry requests from sandboxes fail. Installs
  fail loudly; nothing is fetched unrecorded. This is intended.
- **JWKS unavailable:** requests are refused until keys are fetched; cached keys
  keep working through a refresh failure.
- **Database down:** `503`, nothing forwarded.
- **Upstream registry down or slow:** its status, or `502`/`504`, is returned to
  the sandbox; the request is already recorded.

## Tests

- **Unit, no network:** the Vercel policy in the rules format — plain hosts with
  empty rule lists, registries with exactly one `forwardURL` rule and **no
  `match`**, no hosts still `deny-all`, a bad `forwardURL` refused; the runner
  manager's staging and production refusal without a proxy URL. For the proxy,
  against a test issuer (its own RSA key served as a JWKS) and a fake upstream:
  missing, forged, expired, wrong-issuer, wrong-project and wrong-audience
  tokens refused, and **nothing forwarded or recorded**; a host mismatch, a host
  off the list, `http`, and a non-443 port refused; an unknown or ended runner
  refused; the query never recorded; `Vercel-*`/`Cf-*`/hop-by-hop headers never
  sent upstream; a redirect returned, not followed; a record failure means no
  forward.
- **Integration, real PostgreSQL:** the definer function returns only a live
  runner's session and workspace; the table's RLS and append-only triggers.
- **Live acceptance, opt-in (`vercel_live`):** the egress matrix re-run under
  the rules format; from inside a sandbox, `npm view` of a package goes through
  the proxy to the real registry, succeeds, and appears as a row for that
  session with its path; a request that no rule matches cannot exist, because
  none has `match`; and a request straight to the proxy with no token, or a
  token for another route, is refused.

## Carried forward

**From M5.4b:** measure the provider before believing its reference — every
fact above came from the spike. Judge reachability by response, never by a
connect. Surface every teardown error.

**From M5.3:** a record written before the work is what makes it a guarantee;
the ingestor's rule that nothing is acknowledged until it is stored is the same
rule here.

### Check when done

- A registry request from a Vercel sandbox is recorded with its session and path
  before it is forwarded, and succeeds; one that cannot be recorded is refused.
- No registry request can bypass the proxy: no rule carries `match`.
- The proxy refuses unauthenticated, forged, misrouted and ended-runner
  requests, and fetches only from its registry list.
- Staging and production refuse to start without the proxy.
- ADR-016 written; `architecture.md` lists the new deployment unit; ADR-013's
  threat model updated.
- `make ci` and `make test-integration` pass; the live acceptance test passes,
  recorded in the Verification Record.
