# ADR-019: GCP control plane and BYOK secret store

- Status: Accepted hosting/store direction; deployment and delivery not implemented
- Date: 2026-10-01

## Decision and scope of approval

The owner approved GCP for the Go control plane and BYOK secret storage. Keep
Vercel for the web application and the already-selected isolated Sandbox backend.
This approves the design direction, not cloud provisioning, accessing a customer's
key, installing a CLI or making a paid model call. Region, project identifiers,
service identities, quotas and deployment spending require explicit configuration
and acceptance before staging.

## Deployment shape

- **Cloud Run services:** HTTP API and stateless registry/egress proxies, each
  with its own service identity. Source, prompts and request bodies stay out of
  default infrastructure logs. Existing authentication/RLS/proxy rules remain.
- **Always-on compute:** Temporal workflow workers, runner manager and NATS need
  process availability independent of HTTP traffic. Initial implementation targets
  Compute Engine rather than Kubernetes or request-driven Cloud Run workers.
  Production availability, persistence/backups, patching and recovery are gates;
  a lone development VM is not a production HA claim. Temporal server hosting
  (managed vs operated) still needs a separate operational decision.
- **Privilege separation:** runner manager alone gets BYOK secret access. Do not
  place less-trusted workers or NATS on an instance sharing its attached service
  account/metadata credentials. Use separate instances or a demonstrated
  per-workload identity boundary. Containers alone on one VM do not establish it.
- **NATS:** service listener private; runner-facing WebSocket TLS on the one
  public 443 hostname, with existing listener and subject restrictions (ADR-014/015).
- API/worker database-role separation remains the recorded M9 hardening gate;
  selecting GCP does not solve or waive that pre-existing permission concern.

No region is selected here. Proximity to the configured Vercel Sandbox region,
customer location and data-processing requirements must inform that decision.
No changes to the Vercel Pro/survival/terms staging gates.

## Secret storage and identities

Use **Google Secret Manager** for customer-owned provider API key values and its
managed encryption, not custom application crypto. Environment-separated projects
and service accounts are required. Customer-managed KMS keys are a later enterprise
option, not assumed necessary for the first implementation.

- Product PostgreSQL stores workspace/provider binding, immutable approved secret
  version reference, lifecycle state and audit attribution, never key values.
- Select explicit numeric secret versions, never `latest` or user-chosen aliases.
  Secret resource IDs are opaque and must not be reused after deletion.
- A separate trusted onboarding identity provisions/updates designated secrets;
  manager retrieval uses an attached service account and secret-level access.
  No project-wide default secret-access grant to the API, worker, proxies, NATS,
  browser or sandbox. Avoid service-account JSON key files and implicit operator
  ADC/login fallback in staging/production.
- Secret naming/labels help inventory but are not authorization. Registration must
  verify the trusted environment/project/workspace/provider mapping. A workspace
  admin cannot bind an arbitrary resource from another tenant just by supplying
  its name. Secret metadata and resource names are not public API responses.
- Initial key onboarding stays out-of-band; no browser key-entry/readback UI or
  key-ingestion API is approved/implemented. Providing a key is not paid-call consent.

Secret Manager outages/denials fail closed with bounded deadlines and safe error
categories. No fallback to environment keys, local Claude login or another version.
IAM and quota behavior need actual sandbox verification before retrieval is enabled.

## Delivery and revocation limits

Follow spec 29 for immutable binding versions, allocation snapshots and fenced
handoff. Perform external secret/backend I/O outside database transactions. Check
live binding epoch, session state and exact runner before retrieval and again before
authorizing delivery. Never return a key from a Temporal activity or send it through
outbox, NATS, audit, response caches, argv, repository files or image/snapshot data.

**Revocation is not distributed atomic erasure.** It blocks new authorizations
at the committed epoch and schedules teardown of affected runners. A delivery
already authorized/in flight can race with cancellation; late success must not
mark a cancelled allocation successful and cleanup must converge. Already-delivered
long-lived API keys can remain usable until customer-side provider revocation.
Do not promise instant recall or exactly-once external delivery after an ambiguous
backend timeout. Those cases require an idempotent receiver/receipt and teardown.

Managed sandbox secret-channel support, lack of content logging, trusted-runner
start gating and absence of repository access to the key need a capability spike.
The initial tool-disabled profile is not proof of protection against same-UID tools.
M7 must separately address tool-enabled credential exposure (ADR-018).

## Alternatives and consequences

AWS Secrets Manager is viable, but GCP hosting plus native workload identity avoids
cross-cloud identity federation for the initial backend. Local files/environment
keys are not a substitute for BYOK isolation. Kubernetes is deferred to avoid an
unnecessary cluster while retaining distinct privileged workloads.

This decision enables planning and vendor-neutral binding implementation; it does
not establish a deployed vault, working credential delivery or production readiness.

## Public references and verification provenance

- https://cloud.google.com/secret-manager/docs/access-control
- https://cloud.google.com/secret-manager/docs/access-secret-version
- https://cloud.google.com/run/docs/securing/service-identity
- https://cloud.google.com/compute/docs/access/service-accounts
- https://cloud.google.com/run/docs/configuring/billing-settings

These are reference links, not live acceptance evidence. Public-document fetches
from this development environment did not complete; current SDK/version/IAM and
hosting behavior must be checked during the implementation spike. No cloud API
was authenticated and no resource was provisioned by this planning change.
