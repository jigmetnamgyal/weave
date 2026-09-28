# ADR-013: The runner execution boundary — isolation, source retention and egress

- Status: Accepted
- Date: 2026-09-28 (decided before M5.4, the first unit to run customer code)

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
port but 443.

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
- A registry on the default list is a channel an agent can reach. It is a
  narrow one: registries do not expose request logs to attackers, and runners
  hold no publish credentials, so they cannot push a package carrying data
  out. An attacker-controlled host is not reachable unless an admin adds it.
- The dev backend is weaker than production and must never be mistaken for it.
  That is enforced by configuration, not by convention.

**Revisit if:** no provider passes the checklist (fall back to Kubernetes +
gVisor); per-minute cost exceeds a cluster's at measured load; a customer
requires source to stay in their own cloud account (bring-your-own-runner);
or install times breach the first-session goal (per-workspace cache).
