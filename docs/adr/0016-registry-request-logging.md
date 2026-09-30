# ADR-016: Registry request logging — a public proxy authenticated by Vercel's sandbox OIDC token

- Status: Accepted
- Date: 2026-09-30 (decided before M5.4c, which implements it)
- Implements: ADR-013's control on the registry side channel

## Context

ADR-013 accepts that a public package registry is a channel an agent can
reach, and names the control: every registry request logged by package path,
per session, through a proxy Weave runs, reached by Vercel's `forwardURL`
network-policy rule. It decides nothing else about that proxy: where it runs,
how it knows which session a request belongs to, or how it tells a real
sandbox's request from anyone else's.

The proxy has to be reachable from Vercel's infrastructure, so it is a public
endpoint. Unauthenticated, it would accept forged records attributing fetches
to any session, and forward requests for anyone who found it.

A spike on 2026-09-30 measured what `forwardURL` delivers (full evidence in
`context/features-specs/21-registry-request-logging.md`). Every forwarded
request carries **`Vercel-Sandbox-Oidc-Token`**, an RS256 JWT signed by
`https://oidc.vercel.com/<team>`, whose claims name the project, the sandbox and
its name, and whose **audience is the exact `forwardURL`**. The sandbox never
sees it: Vercel's firewall adds it on the way out.

## Decision

### A separate deployment unit: `services/registry-proxy`

The proxy authenticates, records, then forwards. It is its own deployment unit,
not a route on the API:

- **Different principal.** The API authenticates people through Clerk and
  authorizes them per workspace. The proxy authenticates _sandboxes_ through
  Vercel, and acts on the runner the token names. Two authentication systems on
  one listener would be two ways to get one of them wrong.
- **Different egress.** The proxy fetches from the internet on a sandbox's
  behalf; the API never should.
- **Different load.** It scales with dependency installs, not with product
  traffic, and a slow registry must not tie up API workers.

### Authenticated by Vercel's token, not by a secret of ours

Each request is accepted only if its token has:

- a valid signature against the team issuer's published keys;
- `iss` equal to our team's issuer;
- `project_id` equal to the runner project;
- `aud` equal to the proxy's own route for the requested host.

The runner is then resolved from `sandbox_name`, which embeds the runner id, and
must be a live Vercel runner.

Nothing is placed in the sandbox or its policy to prove identity. A
secret carried in `forwardURL` would also have worked, since the sandbox cannot
read its own policy. It was rejected because the token already proves more: that
Vercel, for our project, forwarded this request from that sandbox. A path secret
proves only that someone knew a string.

The token lives 24 hours, longer than any request needs, and is not
revocable by us. It is bound to one route by its audience, and is useful only
while its runner is live: an ended runner's requests are refused.

### Fail closed

A request is **recorded before it is forwarded**, and one that cannot be
recorded is refused. Registry rules carry **no `match`**, because a request no
rule matches goes straight to the origin, unrecorded. In staging and production
the runner manager refuses to start without the proxy's URL.

## Alternatives rejected

- **An HTTPS proxy set in the sandbox's environment** (`HTTPS_PROXY`). Tools
  can ignore it, and an agent can unset it. The firewall's forwarding cannot be
  bypassed from inside.
- **Reading Vercel's firewall logs.** No API exposes per-request paths, and it
  would couple the record to one vendor's log format.
- **A pull-through registry mirror.** It would also enforce which packages may
  be fetched, and is ADR-013's revisit, not this control.
- **Hosting the proxy inside the API.** Rejected above.

## Consequences

**Accepted:**

- **A second public endpoint** beside the event ingress. It refuses everything
  without a valid token before touching the database or any registry.
- **Registry installs depend on the proxy.** If it is down, sandboxes cannot
  install packages. That is the price of a record with no gaps.
- **Vercel sees registry traffic in plaintext**, since its firewall terminates
  TLS to forward it. That is public content for the default registries, and a
  reason private registries are deferred.
- **Weave's infrastructure gains an egress point**, restricted to the fixed
  registry list, over HTTPS, following no redirects.

**Revisit if:**

- Vercel changes the token's claims or the forwarding headers. The live
  acceptance test is the tripwire.
- Private registries are allowed.
- A detection step needs more than method, host and path.
