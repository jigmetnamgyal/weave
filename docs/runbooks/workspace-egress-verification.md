# Workspace egress verification — M5.4d

## Status

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

## Completion and escalation

Save sanitized observations and provider references in the spec and tracker.
Do not claim the private-address/rebinding gate closed from a connection failure
without a positive fixture control. If the provider cannot guarantee enforcement,
stop and write an ADR for an alternative boundary; a one-time DNS check at host
creation is not an acceptable workaround. Production policy remains unchanged
until the required enforcement is designed, reviewed and verified.
