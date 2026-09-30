# Workspace egress verification — M5.4d

## Status

**Superseded prerequisite:** the operator approved ADR-017's Weave-enforced proxy
for added hosts. Buying a domain, configuring a private fixture or waiting for
Vercel support is no longer required to start M5.4d.1. The setup instructions
below are retained for optional provider investigation, not the current critical
path. Next: deterministic guarded resolver/dial tests; later, live authenticated
forwarding through development quick tunnels. No enforcement is implemented yet.

The hostname-additions feature is not implemented. The approved product rules
are in `context/features-specs/22-workspace-egress-allowlist.md`. This runbook
tracks the remaining provider-enforcement gate, not a production incident.

## Verified so far

- An explicit IPv4 `subnets.deny` overrides an allowed hostname, verified with
  a responding public server and a control without the deny.
- Omitting the deny makes that live acceptance test fail.
- Vercel's create API rejects the IPv6 CIDR `::/0` as invalid.
- Current live sandbox: IPv6 enabled in the kernel, loopback and link-local
  addresses present, public AAAA answers resolve, but no usable external IPv6
  route; an IPv6 Cloudflare request fails while its IPv4 control succeeds.
  This snapshot is not a provider guarantee or an IPv6 policy assertion.

Live diagnostics are opt-in under `vercel_live`, never in CI. They create real
sandboxes and destroy them, checking for retained snapshots in cleanup.
Do not log tokens, credentials, response cookies or raw request headers.

## Next test fixture — prerequisites

Obtain operator approval for a disposable subdomain controlled by the project,
not a customer domain. Do not silently provision infrastructure or change DNS.
The fixture needs:

1. Authoritative DNS with low TTL and programmable A/AAAA records, able to change
   the same hostname from a public address to a forbidden address. Record when
   answers change and account for resolver caches; client `--resolve` is not a
   substitute for changing the proxy's authoritative answers.
2. A benign HTTPS endpoint that returns a unique fixture marker at the initial
   public address. No credentials or customer data are sent.
3. A controlled, reachable endpoint at a forbidden address in a test network
   reachable by the provider (if supported). A timeout to an absent server proves
   nothing. Do not probe cloud metadata secrets or unknown internal services.
4. If testing loopback inside the sandbox, separate local process traffic from
   traffic forwarded by Vercel's egress proxy. Direct local connections are not
   evidence of an outbound-firewall bypass.

Test the initial public response, switch DNS, establish that the provider sees
new answers, and then verify the forbidden endpoint is denied despite the host
remaining allowed. Repeat for CNAME resolution and supported address families.
Use fresh connections as well as repeated requests to expose caching behavior.
Remove the deny in an isolated control to establish the fixture would otherwise
be reachable; never remove protections from a customer runner. Restore policy
and DNS, destroy all fixtures/sandboxes, and verify snapshots are absent.

## Questions for Vercel support

Use https://vercel.com/docs/sandbox/concepts/firewall as the reference:

- Which IPv6 CIDR syntax is supported by `subnets.deny`? The create API rejected
  `::/0`. Is outbound IPv6 unavailable by contract, or only in the current image?
- Are denied ranges checked against the actual upstream address on every new
  connection, including CNAMEs, changed DNS answers and DNS rebinding?
- Does this enforcement apply to both plain allowed-host rules and forwarded
  registry rules? Where is DNS resolution performed for each?
- What is the supported way to test denial against a controlled private fixture
  without probing provider infrastructure or accessing metadata?

## Operator next steps

### 1. Ask Vercel before provisioning a fixture

Open a support request through your Vercel account's available support channel.
If the Hobby account cannot open a private ticket, use the Vercel Community
support channel with only the sanitized technical details below. Do not upgrade
plans solely for this test without explicit cost approval.

Suggested message:

> **Subject: Sandbox firewall — private-address denial, DNS rebinding and IPv6**
>
> We use Vercel Sandbox for untrusted coding-agent workloads. Our rules-format
> policy allows exact hostnames and forwards selected public-registry requests
> using `forwardURL`. We want to allow workspace admins to add exact hostnames,
> while always denying private, loopback, link-local and metadata destinations.
>
> In live tests, `subnets.deny: ["0.0.0.0/0"]` overrides an allowed hostname.
> However, sandbox creation via `/v4/sandboxes` rejects `subnets.deny: ["::/0"]`
> with HTTP 400, `Invalid CIDR "::/0"`. The current sandbox resolves public AAAA
> records but has no usable external IPv6 route; we do not assume that is a
> permanent security guarantee.
>
> Could you confirm:
>
> 1. The supported IPv6 deny syntax, or whether outbound IPv6 is unavailable
>    by contract across supported regions/images.
> 2. Whether deny ranges are enforced against the final upstream address on
>    every new connection, including CNAME chains and DNS rebinding after an
>    allowed hostname changes its answers.
> 3. How this applies to plain hostname rules versus `forwardURL` rules, and
>    where each upstream is resolved.
> 4. A supported safe verification fixture: an endpoint at a forbidden address
>    that we can demonstrate is reachable in an isolated control without the
>    deny, then blocked with it. We do not want to probe your internal services
>    or fetch metadata credentials. Is Secure Compute required for such a
>    fixture, or can your team provide equivalent verification evidence?
>
> Please link the supported enforcement contract/documentation where possible.

Share the answer, not account credentials or tokens. Provider guarantees and
live observations must be recorded separately; a support answer does not mean
a controlled test has run.

### 2. Identify a disposable DNS name if a fixture is feasible

Tell the implementation agent the DNS provider and a proposed unused hostname,
for example `egress-check.<your-domain>`. Do not paste DNS API tokens into chat.
Use an isolated test name; do not change apex, mail, application, or production
records. The test plan must specify endpoint placement, TLS handling, TTL/cache
observations, positive controls and rollback before any DNS mutation.

Cloudflare quick-tunnel names used by local development cannot be switched to
arbitrary A/AAAA records by this project, so they are not a rebinding fixture.
A third-party wildcard resolver also does not provide a controlled same-name
answer transition or a reachable private endpoint.

### 3. Resume implementation after the gate closes

Once enforcement is supported and verification is sufficient, write the policy
ADR/amendment, then build the contracted tenant-scoped store/API/audit boundary,
followed by runner policy composition and UI. If the fixture requires paid
infrastructure, present the cost and alternative evidence before creating it.

## Completion and escalation

Save sanitized observations and provider references in the spec and tracker.
Do not claim the private-address/rebinding gate closed from a connection failure
without a positive fixture control. If the provider cannot guarantee enforcement,
stop and write an ADR for an alternative boundary; a one-time DNS check at host
creation is not an acceptable workaround. Production policy remains unchanged
until the required enforcement is designed, reviewed and verified.
