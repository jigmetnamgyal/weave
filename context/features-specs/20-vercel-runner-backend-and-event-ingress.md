Read `CLAUDE.md` before starting

We're moving the runner onto its production isolation boundary (Unit M5.4b):
a `RunnerBackend` for **Vercel Sandbox**, and the event ingress that lets a
runner outside Weave's network reach NATS (ADR-015). When it is done, a
session runs end to end inside a Firecracker microVM with deny-by-default
egress — the first time a Weave runner is somewhere untrusted code could
safely run.

Read `docs/adr/0013-runner-execution-boundary.md`,
`docs/adr/0014-nats-credential-model.md`,
`docs/adr/0015-runner-event-ingress.md` and
`context/features-specs/17-runner-manager-and-dev-runner.md` first. This unit
implements the port M5.4a defined, against the checklist ADR-013 wrote, with
the transport ADR-015 decided.

## M5.4b is split before building, and this is the first half

The tracker's M5.4b was "verify providers, the production adapter, the
hostname-enforcing egress proxy with the default allowlist, and self-serve
workspace allowlist additions". The provider evaluation changed what the
rest of that line means:

- **Vercel's firewall is the hostname-enforcing proxy.** ADR-013 accepts
  either a provider hostname allowlist or a proxy Weave operates; the first
  passed every egress test (below). Weave does **not** build a proxy to
  enforce the allowlist.
- **What still needs a Weave-operated proxy is ADR-013's registry control**:
  logging every registry request by package path, per session, required
  before M6. Vercel's firewall sees only the hostname. The way to see paths is
  its `forwardURL` rule, which terminates TLS and hands each request to a
  proxy Weave runs — a different component, with its own trust boundary.
- **Workspace allowlist additions** are an API, a table, an audit trail, a
  cap and a UI: a control-plane slice, not a runner-plane one.

So:

- **M5.4b (this unit)** — the Vercel `RunnerBackend`, the default allowlist
  through Vercel's firewall, and the event ingress.
- **M5.4c** — registry request logging through `forwardURL`, and workspace
  allowlist additions. Both before M6.

## Why Vercel: the evidence

Four providers were read against ADR-013's checklist; two were tested live on
2026-09-29 with throwaway scripts against working accounts (results in the
Verification Record). Daytona was refused on its documentation: its default
sandbox is a namespaced container on a shared kernel. E2B was refused on its
documentation: allow rules override deny rules, and a public DNS server is
always allowed when domains are, which is a DNS exfiltration channel.

| Checklist item | Vercel Sandbox | Modal |
| --- | --- | --- |
| microVM / gVisor | Firecracker: own guest kernel, hypervisor flag | gVisor — **only if pinned**; unset, Modal picks |
| Deny-by-default, hostname allowlist | Pass: other hosts, subdomains, port 80, raw IPs, metadata, private ranges and port 22 refused | Pass for TCP |
| SNI pointed at another address | Reaches the real host: Vercel resolves the name itself | Same |
| DNS as a channel | **Closed**: unlisted names do not resolve, UDP/53 unanswered | **Open**: any name resolves under every configuration tried |
| Credentials outside the VM | Brokering works **with** an allowlist | Header replacement is refused beside any allowlist or `blockNetwork` |
| No inbound exposure | No route without requested ports | No tunnels without requested ports |
| Several hours | 24 h on Pro; **45 min on Hobby** | 5 h accepted on Starter |
| Teardown by API / on lost heartbeat | `stop`/`delete`; an un-extended sandbox stopped itself at its timeout | `terminate`; `idleTimeoutMs` plus a periodic exec; one sandbox survived cleanup once, not reproduced |
| Region | `fra1` honoured; 19 regions | `eu-west-1` accepted; 1.15–1.75× price |
| Secrets not on disk | Not found anywhere after a per-command secret | Not found |

Modal fails deny-by-default egress on DNS, and ADR-013 refuses a provider
that fails any item. Vercel is also roughly half the per-session cost once
paid for (about $0.055 against $0.12 for a 30-minute, 2 vCPU session).

## Operator prerequisites — none block building; three block staging

**Development runs on Vercel Hobby** (decided 2026-09-29). Everything this
unit builds and tests works there — the spike ran on Hobby — and the fake
provider's sessions last seconds to minutes. Hobby's limits: a 45-minute
sandbox, 10 concurrent sandboxes, and a monthly allowance (5 active-CPU
hours, 420 GB-hours of memory, 5,000 creations) past which creation
**pauses** until the next cycle rather than billing. A live test that fails
with creation refused has hit the allowance, not a defect. **Unconfirmed, on
the operator:** Vercel's terms restrict Hobby to personal, non-commercial use,
and its fair-use guidelines count a deployment made for anyone's financial
gain as commercial. Whether developing Weave on Hobby qualifies is not yet
settled. If it does not, Pro moves ahead of this unit's live tests, not only
ahead of staging; nothing in the code changes either way.

`cloudflared` is installed (2026.9.3), and a quick tunnel reached a local
server from its public hostname on 2026-09-29.

**Before staging, and closing Vercel's acceptance under ADR-013:**

1. **Vercel Pro**, for its 24-hour session cap; a session may run
   `application.SessionMaxRunTime` (eight hours) plus its drain.
2. **The one live test that has not run**: one sandbox kept alive by
   heartbeat for longer than `SessionMaxRunTime` plus the drain bound —
   about 8.5 hours, under a dollar. Needs Pro. Record it in the Verification
   Record.
3. **Vercel's data-processing terms**, read and accepted for customer source.

And a decision on Open Question 8 before anything is deployed.

## The Vercel backend

`internal/adapters/vercelsandbox`, implementing `application.RunnerBackend`
over Vercel's **REST API** — there is no Go SDK. Every call the port needs
exists: create, get and list named sandboxes, stop, delete, extend a
session's timeout, update its network policy, execute a command, and get a
command's status and exit code. **Read the current REST reference, not the JS
SDK**: the spike used the SDK and found at least one parameter named
differently at the HTTP layer (`project`, not `projectId`, on list).

### Non-persistent, always

**Vercel sandboxes are persistent by default**: on stop the filesystem is
snapshotted and kept for 30 days, and deleting the sandbox does not delete
its snapshots. That is source retained after teardown, which ADR-013
forbids. Every create sets `persistent: false`, and **teardown verifies no
snapshot exists** for the sandbox before it reports success — a test holds
the create request's shape, and the live test holds the outcome.

### Names, tags, and what this backend owns

- The sandbox **name is derived from the runner id**, the way the dev backend
  names containers. Names are unique per project, so a retried `Provision`
  finds the sandbox it made rather than making a second, and `HandleFor`
  is a get by name.
- **Tags** carry the runner id, session id and a scope, as the dev backend's
  labels do. `List` filters on the scope tag — the API accepts one tag
  filter — so a developer and a staging deployment sharing a project cannot
  see each other's sandboxes. Separate projects per environment are still the
  rule (`architecture.md`: separate accounts and credentials per
  environment); the tag is the second layer.
- Every environment's project holds **only runner sandboxes**, and the
  token is scoped to its team. The spike's token could not read team details,
  which is the least-privilege result, not a fault.

### Starting the runner

- **The runner process is started as a detached command**, with its secrets
  — the git token and the NATS credential — in **that command's environment**,
  never in the sandbox's create-time environment. Create-time environment is
  sandbox configuration, returned by the API and inherited by every later
  command, including an agent's in M6. The spike found a per-command secret
  nowhere on the filesystem; the runner's re-exec (M5.4a) then moves it out
  of `/proc/<pid>/environ` as before.
- **Readiness** is the runner's own `healthcheck` subcommand, executed
  through the command API; **exit code** is the start command's.
- **The image. Decide and record:** a runner image in Vercel Container
  Registry, or the default image plus the runner binary written in at
  provision. The image is what `architecture.md`'s signing and admission
  requirements attach to; the binary upload needs no registry. Either way,
  verify the sandbox's architecture before building for it — the spike did
  not check `uname -m`.
- **Non-root, and no sudo.** Vercel sandboxes run as a user with sudo
  available. The runner runs as a dedicated user created without it, and a
  test proves `sudo -n true` fails as that user. M6's agent will run as a
  second user, so the runner's credentials are not readable by the agent's
  processes; this unit only has to leave room for that.

### The network policy

Built by the runner manager, never from input, and always a **custom
allowlist** — never `allow-all`:

- `github.com`, `codeload.github.com` (ADR-013);
- the registry hosts ADR-013 lists;
- the **event-ingress hostname** (ADR-015), as the system's entry;
- the model provider's host is M6's to add; the fake provider needs none.

**Exact hostnames only, no wildcards.** Vercel's matching is SNI-only and
does not stop domain fronting on shared CDNs; narrow, single-purpose
hostnames are the mitigation its documentation gives. Note also: **an empty
policy is `deny-all`**, including DNS — fine here, but a policy builder that
drops every entry fails closed rather than open, and a test should show it.

### Leases: teardown on lost heartbeat

A sandbox is created with a **short timeout — a lease** — and the runner
manager extends it while the runner's session is live. If the runner manager
stops, leases lapse and sandboxes stop on their own within one lease; the
spike measured a 90-second sandbox stopping itself at 96 seconds. **Decide
and record** the lease length and where the extension runs. The reconciler
already visits every live runner every 30 seconds, and a runner manager that
is down extends nothing, which is the property wanted.

**The hard ceiling is the plan's session cap, so it is configuration**
(`RUNNER_VERCEL_MAX_SESSION`): 45 minutes on Hobby, 24 hours on Pro. No
lease or timeout the backend requests may exceed it. **In staging and
production the runner manager refuses to start** when the cap is below
`SessionMaxRunTime` plus the drain bound — a test holds that, for both
sides of the boundary. In development a cap below it is allowed: a session
that outlives it loses its sandbox and fails through the lost-runner path,
which is harmless with the fake provider and says so in its reason.

### Region

`RUNNER_VERCEL_REGION`, default `iad1`, until Open Question 8 is answered.
Region changes Vercel's CPU and memory rates, not the design.

### Configuration

`RUNNER_BACKEND=vercel`, with the token, team id, project id, region and
maximum session (`RUNNER_VERCEL_MAX_SESSION`, above). In
development they come from `.env`; anywhere else an unset value fails
startup, and they come from the secret store (M9). They are never logged.

## The event ingress (ADR-015)

- **NATS gains a WebSocket listener.** In development: `ws://`, no TLS, on a
  published local port, for the dev backend's runners and for the tunnel. In
  the model production will use: TLS on 443 at a dedicated hostname.
- **Every runner connects over WebSocket**, on both backends:
  `RUNNER_NATS_URL` becomes a `ws://` URL in development and `wss://` for
  Vercel. `nats.go` accepts both without code changes; confirm
  the M5.5a start-up scope proof still passes over it.
- **`internal/adapters/natsauth`** sets `AllowedConnectionTypes`:
  `WEBSOCKET` for runners, `STANDARD` for every service identity, both for
  tests.
- **Limits**: a maximum payload sized from the encoded-event limit,
  `domain.MaxEventBytes` (64 KiB, `contracts/events`), plus headroom for
  message headers — NATS counts headers against it, and every runner publish
  carries `Nats-Msg-Id` — so no event the ingestor would accept is refused at
  the broker. A field cap such as `message.created`'s 32,768-character text
  is not the bound. Also a small subscription cap for runner users, and an
  account connection limit, which counts only authenticated connections.
  Before authentication: a short authentication timeout and the WebSocket
  handshake timeout here, and a per-source limit in front of the listener
  wherever it is hosted (ADR-015) — not the server-wide connection limit,
  which the internal listener shares.
- **Development tunnel: Cloudflare Tunnel** (decided 2026-09-29). A Vercel
  sandbox cannot reach a laptop, so testing this backend against a local
  stack needs a public 443 hostname forwarded to the local WebSocket
  listener; `cloudflared` provides one and proxies WebSocket. Cloudflare
  terminates TLS, which ADR-015 allows **in development only**, with
  synthetic repositories. **Decide and record** quick tunnel or named
  tunnel: a quick tunnel needs no account but gets a new `trycloudflare.com`
  hostname every start, so the runner manager must read the ingress
  hostname from configuration each run (and `make` can capture it); a named
  tunnel keeps one hostname but needs a Cloudflare account and a domain on
  it. Either way the hostname is an exact entry on the sandbox's policy,
  never a wildcard over `trycloudflare.com`.
- **Start the tunnel first, and wait for its name to resolve.** A new
  quick-tunnel hostname takes seconds to appear in DNS, and a lookup made
  before then is cached as not-found: on 2026-09-29 the first check failed
  for exactly that reason and every retry failed with it, until the name was
  resolved through 1.1.1.1. So whatever starts the tunnel waits for
  `Registered tunnel connection` and for the hostname to resolve publicly
  **before** the runner manager provisions a sandbox that will look it up.

## Non-scope

- Registry request logging and workspace allowlist additions — M5.4c.
- Hosting production NATS, its hostname and certificate — staging, gated on
  Open Question 8.
- A bearer credential injected by Vercel so the runner never holds it —
  ADR-015's M6 revisit.
- Active revocation of runner credentials — ADR-014's M6 revisit.
- Any provider but the fake one.

## Security

- The trust boundary moves: untrusted code will run on Vercel's
  infrastructure, and Weave's only inbound surface from it is the event
  ingress. Record the threat-model change the protected-files rule asks for
  when the egress policy and secret injection change — here, both do.
- The Vercel token can create sandboxes that reach the internet; it lives
  only in the runner manager, like the NATS signing key.
- Nothing in this unit loosens ADR-014's runner scope or the ingestor's
  binding.

## Failure behavior

- **Vercel unreachable or refusing at provision**: bounded retries, then the
  session fails with a reason naming the backend — distinct from GitHub or
  checkout failures.
- **Rate limited** (429): backoff within the activity's bounds. Pro allows
  10,000 control-plane requests a minute and 20 deletions a second; status
  polling counts against the first.
- **A lease that cannot be extended**: the sandbox stops, the runner exits,
  and the session fails through the existing lost-runner path — never left
  `running` with nothing behind it.
- **Teardown that cannot confirm no snapshot**: retried, and alerted on if it
  persists — a retained snapshot is retained source.

## Tests

- **Unit, no network**: the create request always carries `persistent:
  false` and a custom policy with exactly the expected hosts; secrets appear
  only in the start command's environment; names derive from runner ids;
  `Provision`, `Destroy` and `HandleFor` are idempotent against a fake HTTP
  server; 404 on destroy is success; a policy with no entries is `deny-all`.
- **Integration, local NATS**: a runner credential is refused on the
  standard port; a service credential is refused on the WebSocket listener;
  an unauthenticated WebSocket connection is refused; a runner's scope over
  WebSocket is exactly its scope over the standard port today. The M5.3,
  M5.4a and M5.5 integration tests pass with runners on WebSocket, unchanged
  in what they assert.
- **Live acceptance, opt-in** — a build tag, Vercel credentials required,
  never in CI (the testing rules forbid network-dependent passing tests).
  Port the spike's checks to Go: the egress matrix, no snapshot after stop
  and after timeout, a lapsed lease stopping the sandbox, a secret absent
  from disk, the runner user without sudo. Then **one session end to end**:
  created through the API, run on Vercel against the local stack through the
  tunnel, five fake-provider messages in history, `review_ready`, nothing
  quarantined, nothing left in the project.

## Carried forward, and worth re-reading before starting

**From the provider spike.** Blocked connections can look open at the TCP
layer on these platforms; judge reachability by an HTTP response or a
protocol banner, never by a successful connect. The spike's Modal cleanup
swallowed errors and so could not say why one sandbox survived — surface
every teardown error.

**From M5.4a.** Teardown on every path out, the reconciler for what a crash
leaves, and `os.Unsetenv` not removing a value from `/proc/<pid>/environ`.

**From M5.2 and M5.4a, on interference.** A running `make dev` absorbed test
work twice. A live test and a developer's stack must not share a Vercel
project's scope tag.

### Check when done

- A session runs end to end on Vercel Sandbox, and nothing of it remains in
  the project afterwards — no sandbox, no snapshot.
- The sandbox refused every destination not on its policy, in a recorded
  live run.
- Runner and service credentials are each refused on the other's listener.
- The image choice, lease length, extension placement and quick-or-named
  tunnel are recorded, with reasons.
- The whole unit is verified on Hobby. Pro, the 8.5-hour survival run and
  the DPA are tracked as the staging gate that closes Vercel's acceptance
  under ADR-013 — recorded when done, not required to finish this unit.
- `make ci` and `make test-integration` pass; the live acceptance test
  passes and its output is in the Verification Record.
