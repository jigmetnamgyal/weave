# ADR-013: The runner execution boundary — isolation, source retention and egress

- Status: Accepted
- Date: 2026-09-28 (decided before M5.4, the first unit to run customer code)
- Amended: 2026-09-29 by ADR-015 (runner event ingress); provider selected
  the same day (see "Provider selection" below); 2026-09-30 by M5.4b's
  implementation (see "Threat model after M5.4b" below)

## Context

M5.4 provisions the first environment that executes untrusted input: a
customer's repository, its dependencies, the task text, and a coding agent's
output. `context/architecture.md` already fixes the shape — one ephemeral
sandbox per session, gVisor or microVM isolation, deny-by-default egress, no
inbound exposure, no privileged Docker socket — and left three things open,
each tracked as an Open Question gated on M5.4:

- which isolation technology, and who operates it;
- whether a copy of the customer's source outlives the session;
- what a runner may reach on the network by default.

They are one decision seen from three sides: where untrusted code runs, what
it leaves behind, and where it can send things. They are recorded together
for that reason.

The deciding constraint is not technical. Weave is built and operated by one
person at this stage, for teams of 3–20 developers, and the first-session goal
is under ten minutes. A boundary that is strong on paper and operated alone at
3 a.m. is weaker than a narrower one someone else runs.

## Decision

### 1. Isolation: a managed sandbox provider, behind a port

Runners execute on a **managed isolated-compute provider** offering microVM or
gVisor isolation, reached through a `RunnerBackend` port in the runner
manager. Kubernetes with gVisor (for example GKE Sandbox) is the documented
path if cost at scale or a provider's limits make it worth operating a
cluster; self-run Firecracker or Kata is not planned.

**No provider is chosen by this ADR.** Capabilities and terms change, and
choosing from memory would be choosing wrong. A provider is accepted only if it
passes every item below, verified against its current documentation and a
working account — and **egress is the item most likely to fail**:

- deny-by-default outbound networking, with either a hostname allowlist or the
  ability to force all traffic through an egress proxy Weave operates;
- no inbound exposure from the public internet;
- microVM or gVisor isolation, not a shared-kernel container;
- sessions lasting several hours without forced termination;
- explicit teardown by API, and teardown on lost heartbeat;
- an available region consistent with Open Question 8 (initial hosting
  region), and data-processing terms acceptable for customer source;
- secrets injectable at runtime without being written to the workspace volume.

A provider failing any item is refused, however convenient.

**Provider selection (2026-09-29).** Vercel Sandbox, Modal, E2B and Daytona
were read against this checklist, and Vercel and Modal tested live. **Vercel
Sandbox is selected.** Modal is refused on the first item: names outside its
domain allowlist still resolve, a DNS exfiltration channel. E2B is refused on
the same ground from its documentation, and Daytona's default sandbox shares
the host kernel. Vercel's acceptance completes when the three items still
open are recorded — a Pro plan (Hobby caps a sandbox at 45 minutes), one
sandbox surviving past the session's maximum run time, and its
data-processing terms. Evidence and the open items:
`context/features-specs/20-vercel-runner-backend-and-event-ingress.md`.

**Local development cannot use the production boundary**: macOS runs neither
gVisor nor Firecracker. A local backend — hardened Docker: non-root, dropped
capabilities, read-only root, no host mounts — exists for development and is
**not a security boundary**. It does not enforce the egress policy below; a
Docker network cannot express a hostname allowlist, and the egress proxy that
can arrives with the production backend. It is named as such
in code, and the runner manager refuses to start it when `APP_ENV` is
`staging` or `production`, so it cannot be deployed by accident.

### 2. Source retention: deleted at teardown

**No copy of a customer's source outlives its session.** The sandbox, its
checkout and its writable volumes are destroyed on completion, cancellation,
timeout or lost heartbeat. What remains is derived: the patch, redacted logs,
test reports and — from M8 — the pull request.

A snapshot is not needed to continue work. GitHub holds the source, and M5.2
records the exact commit each session branched from (`sessions.branch_sha`), so
a continuation — invariant 10's linked new session — re-clones at that commit
and applies the stored patch.

**No persistent dependency cache** in the MVP: installs start cold. A cache is
source-adjacent (lockfiles, private packages) and would be retained data; if
install time threatens the first-session goal, a per-workspace cache may be
added under the same deletion rules, never shared across workspaces.

### 3. Egress: deny by default, a curated allowlist, workspace additions

Outbound traffic is **HTTPS on port 443 only, through an egress proxy that
enforces hostnames** — IP-based network policy cannot express
`registry.npmjs.org`. The default allowlist:

- GitHub for git over HTTPS (`github.com`, `codeload.github.com`), scoped by
  the task's installation token;
- the configured model provider's API host;
- public package registries: npm (`registry.npmjs.org`), PyPI (`pypi.org`,
  `files.pythonhosted.org`), Go (`proxy.golang.org`, `sum.golang.org`),
  crates.io (`crates.io`, `static.crates.io`, `index.crates.io`), RubyGems
  (`rubygems.org`), Maven Central (`repo1.maven.org`, `repo.maven.apache.org`).

**Always refused, whatever a workspace adds:** cloud metadata endpoints
(`169.254.169.254` and equivalents), private, loopback and link-local ranges,
Weave's own internal services, destinations given as raw IP addresses, and any
port but 443. _(Amended by ADR-015: the runner event ingress — a public NATS
WebSocket listener on 443 that accepts only runner credentials — is not an
internal service, and is the one Weave-operated hostname on every sandbox's
allowlist. Everything else Weave runs stays refused.)_

**Workspace additions are self-serve for admins** — `workspace:manage`, the
permission that already governs agent profiles — audited, capped in number,
and hostname-only (no wildcards across a registrable domain). An addition
applies to that workspace's sessions only.

## Consequences

**Accepted:**

- A vendor dependency for the most sensitive component, contained by the
  `RunnerBackend` port so that moving to Kubernetes + gVisor changes one
  adapter, not the product.
- Per-session-minute cost that is higher than a cluster at steady load. At the
  MVP's scale, engineering time is the scarcer resource.
- Cold dependency installs. Slower first builds are the price of promising
  that source is not retained.
- A registry on the default list is a channel an agent can reach. Runners
  hold no publish credentials, so they cannot push a package carrying data
  out, and an attacker-controlled host is not reachable unless an admin adds
  it. **One residual channel remains, and is accepted with a control:** a
  public registry reports per-package download counts, so an agent induced to
  fetch attacker-owned packages — `leak-a`, `leak-b`, in an order or a count —
  signals out a few bits per request through numbers anyone can read. It is
  low-bandwidth and noisy — counts include mirrors and bots, and update with a
  delay — but not bounded: repeated requests can move data in chunks, so a
  small file is slow to exfiltrate this way, not impossible. The first version
  of this ADR wrongly said registries expose nothing, and the second wrongly
  said the channel could not carry a file. **Control, required in M5.4c, before
  M6:** a Weave logging proxy, reached through Vercel's `forwardURL` rule,
  logs every registry request with its package path per session, so a session
  fetching packages no manifest in its repository names is visible and
  alertable. It is not the enforcement: M5.4b's hostname allowlist is enforced
  by Vercel's firewall, and the proxy only observes what that allowlist lets
  through. **Revisit** — with a pull-through
  registry mirror resolving only packages the repository's lockfiles name —
  if a customer's threat model includes a determined insider or high-value
  source.
- The dev backend is weaker than production and must never be mistaken for it.
  That is enforced by configuration, not by convention.

**Revisit if:** no provider passes the checklist (fall back to Kubernetes +
gVisor); per-minute cost exceeds a cluster's at measured load; a customer
requires source to stay in their own cloud account (bring-your-own-runner);
or install times breach the first-session goal (per-workspace cache).

## Threat model after M5.4b (2026-09-30)

M5.4b changed the egress policy and secret injection, which the project's
protected-files rule requires to be recorded here. What moved, what was
verified live on Vercel Hobby, and what M6 inherits:

- **Where untrusted code runs.** On Vercel's Firecracker microVMs, not a
  container on a shared kernel. Weave's only inbound surface from a sandbox
  is the event ingress (ADR-015); a runner credential is refused on the
  internal NATS port and every service credential on the public listener,
  verified through the development tunnel.
- **Egress, enforced and measured.** Every sandbox gets an explicit policy of
  exact hostnames — the defaults above plus the ingress — never `allow-all`
  and never a wildcard; an empty policy is `deny-all`. Live: the defaults and
  the ingress answered; an unlisted host, an unlisted subdomain of an allowed
  one, port 80, a raw IP, cloud metadata, a private address and port 22 did
  not; an unlisted name did not resolve; and an allowlisted SNI aimed at
  another address still reached the real host.
- **Secret injection.** The git token and the broker credential travel in
  exactly one API request — the runner's start command's own environment —
  never in the sandbox's create-time environment (inherited by every later
  command), never in arguments (returned by the API), never in an error
  message. They transit Vercel's API, which is accepted: the provider already
  holds the environment they are used in. A search of the whole filesystem,
  as root, found neither.
- **Privilege inside the sandbox.** Vercel's default user has passwordless
  sudo. The runner runs as `weave-runner`, created without it, entered
  through `setpriv --no-new-privs`: live, `sudo` failed for it with and
  without no-new-privs, and it held no capabilities. **M6 must run the agent
  as a third user, neither the default user nor the runner's**, so the agent
  can neither sudo nor read the runner's credentials.
- **Retention.** Sandboxes are created `persistent: false`; teardown verifies
  and removes any snapshot. Live: none after a lapsed lease, an explicit stop,
  or deletion, and none in the project after a full session.
- **Teardown on lost heartbeat.** A sandbox lives on a five-minute lease the
  runner manager extends; live, an unextended one-minute lease stopped its
  sandbox at 59 seconds.
- **Found live, not in the reference:** a command's exit is reported only to
  a read with `wait=true`; the adapter reads it that way, bounded, so a
  finished runner is never mistaken for a running one.
- **Open, on the staging gate:** a command's optional timeout is capped at
  five hours, and a session may run eight. The runner is started without one;
  the nine-hour survival run must show it is not killed at five.
