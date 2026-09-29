# ADR-015: Runner event ingress — a public NATS WebSocket edge that only runners can use

- Status: Accepted
- Date: 2026-09-29 (decided before M5.4b, the first unit whose runner runs
  outside Weave's own network)
- Amends: ADR-013 (the always-refused list), `context/architecture.md`
  (NATS subjects are accessible only to service identities)

## Context

A runner publishes its session's events to NATS JetStream (ADR-004) with a
credential scoped to one subject (ADR-014), and waits for the stream to
confirm each one before counting it sent (M5.5b). In development the runner
is a Docker container on the same host as NATS and reaches it on the
standard client port.

M5.4b moves the runner onto a managed sandbox provider — Vercel Sandbox,
selected against ADR-013's checklist on 2026-09-29 (evidence in
`context/features-specs/20-vercel-runner-backend-and-event-ingress.md`). From
there the only way out is ADR-013's egress rule: **HTTPS on 443, to a
hostname on the allowlist**, with Weave's own internal services always
refused. Three facts make the current connection impossible:

- **The provider's firewall matches on the TLS SNI** and forwards without
  terminating TLS. It needs a hostname in the first bytes of the connection.
- **The NATS client protocol sends a plaintext `INFO` before TLS starts.**
  There is no SNI to match, so the connection cannot pass a hostname filter
  at all, on any port.
- **NATS is an internal service**, and ADR-013 refuses those whatever a
  workspace adds.

`context/architecture.md` also still says NATS subjects "are accessible only
to service identities". ADR-014 already made that untrue — runners hold
credentials — without amending it.

## Decision

### Runners reach NATS through one public WebSocket listener

NATS's WebSocket listener, **TLS on 443, at one dedicated hostname** (for
example `events.<domain>`), is the only part of NATS reachable from outside
Weave's network. To a hostname-enforcing firewall it is ordinary HTTPS, and
`nats.go` accepts a `wss://` URL with no other change: the runner's
connection code, credential and publish path stay as M5.5a and M5.5b built
them. The standard client port is **never** exposed; services keep using it
inside the network.

**Every runner connects this way, on every backend.** The dev backend's
runners use a local `ws://` listener rather than the standard port, so there
is one runner connection path to test and the connection-type rule below
holds everywhere.

TLS for the public listener terminates at NATS itself or at a load balancer
Weave operates, **never at a third party** — event payloads carry agent
output, which may quote customer source. The one exception is development,
where a tunnel may terminate TLS at its provider because the repositories are
synthetic.

### The credential decides which listener it may use

ADR-014's user JWTs gain an allowed connection type, enforced by the server:

| Identity                      | Allowed connection types                     |
| ----------------------------- | -------------------------------------------- |
| Runner                        | `WEBSOCKET` only                             |
| Ingestor, runner manager, API | `STANDARD` only                              |
| Tests                         | both — their own identity, never a service's |

A service credential that leaks cannot be used from the internet, because the
only public listener refuses it. A runner credential cannot be used on the
internal port. Unauthenticated connections are refused on both, as they are
today.

A runner's **scope is unchanged**: publish only its own session's subject,
subscribe only its own reply inbox, twelve-hour expiry, and the ingestor's
binding as the second layer (ADR-014, M5.4a).

### The edge is bounded

- **Limits on the public listener and on runner users**: a maximum payload
  no larger than the event contract permits, a small maximum number of
  subscriptions per runner (it needs its reply inbox and nothing else), and a
  connection limit on the account, so an authenticated flood is capped as well
  as an unauthenticated one.
- **The allowlist entry is the system's, not a workspace's.** The runner
  manager adds the ingress hostname to every sandbox's policy itself, as an
  exact hostname with no wildcard. It is not a workspace addition, does not
  count against the cap, and a workspace cannot remove it.

### What ADR-013's refusal now means

"Weave's own internal services" **remain always refused.** The event ingress
is not one of them: it is a public edge that authenticates every connection
and accepts only runner credentials, and it is the **one Weave-operated
destination** on a sandbox's allowlist. Nothing else Weave runs — the API,
PostgreSQL, Redis, Temporal, the standard NATS port — becomes reachable.

## Alternatives rejected

- **An HTTPS event gateway** — a service the runner posts events to, which
  publishes to JetStream inside the network. NATS would stay fully private,
  but it is a new deployment unit, a **second authentication system**
  duplicating ADR-014, and it would have to reproduce the stream's
  per-event confirmation that M5.5b's drain depends on. The same outcome for
  more code, and more to get wrong.
- **Streaming events through the provider's command logs** — the runner
  writes to stdout and the runner manager reads it through the provider API
  and publishes. The sandbox would need no route to Weave at all. Rejected:
  it puts the runner manager in every session's data path, loses the runner's
  confirmed publish, and ties event transport to one vendor's API, which is
  the coupling the `RunnerBackend` port exists to prevent.
- **NATS TLS-first on 443** (`handshake_first`). It would also carry an SNI
  and pass the firewall. Rejected in favour of WebSocket because WebSocket is
  plain HTTPS to every load balancer, proxy and tunnel in the path, and
  because it leaves open the credential-brokering option below.
- **The provider's private networking** (Vercel Secure Compute, VPC
  peering). Enterprise-only today, and a transport that works on one vendor.

## Consequences

**Accepted:**

- **A NATS listener reachable from the internet.** Everything before
  authentication — the WebSocket upgrade, the TLS handshake, the `CONNECT` —
  is exposed to anyone. The server is kept patched, and connection and
  authentication failures on the public listener are alerted on.
- **A public hostname and certificate to operate**, and, in development, a
  tunnel to reach a laptop. Where production NATS is hosted waits on Open
  Question 8 (initial hosting region).
- **A runner credential is usable from anywhere until it expires.** Before
  this ADR a leaked credential still needed a network path to NATS. The bound
  is the same as ADR-014's — its own session's subject only, refused by the
  ingestor once the runner is torn down, dead after twelve hours — but it is
  no longer also bounded by the network.

**Revisit when M6 runs a real agent** — the moment ADR-014 already names for
revisiting exfiltration from a live runner:

- **Keep the credential out of the sandbox entirely.** The WebSocket listener
  can read a JWT from a cookie on the upgrade request, and Vercel's
  credential brokering injects headers outside the VM, so the runner could
  publish without ever holding its credential. That needs a _bearer_ JWT —
  one the server accepts without a nonce signature — which trades
  proof-of-possession for never being in the sandbox. Verify both halves with
  a spike before relying on it.
- **Shorten the expiry** to the session's maximum lifetime plus a margin, now
  that the network no longer bounds a leaked credential.
