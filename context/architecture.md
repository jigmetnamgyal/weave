# Architecture Context

## Architecture Strategy

Weave uses a **modular control plane with a separately isolated execution plane**. The control plane begins as a well-structured deployable service rather than a collection of premature microservices. The execution plane is separate from the first release because it runs untrusted customer code and requires a different security, scaling, and failure model.

This design supports an efficient MVP while preserving clear extraction boundaries for enterprise scale. Internal modules communicate through explicit interfaces, and external asynchronous work is coordinated through durable workflows and versioned events.

## Stack

| Layer | Technology | Role |
| --- | --- | --- |
| Web framework | Next.js App Router + TypeScript | Server-rendered application shell, route composition, authenticated product UI |
| UI | React, Tailwind CSS, shadcn/ui, Radix primitives | Accessible interface and design-system foundation |
| Client data | TanStack Query | Cached server state, invalidation, optimistic UI where safe |
| Forms/validation | React Hook Form + Zod | Accessible forms and client-side validation using generated schemas where possible |
| Code/diff | Monaco Editor | Read-only code views and unified/side-by-side diffs |
| Control-plane API | Go | Authorization, business logic, integration management, sessions, billing, and audit APIs |
| HTTP contracts | OpenAPI 3.1 | Versioned API contract and generated TypeScript client |
| Primary database | PostgreSQL | Transactional tenant, task, session, policy, approval, billing, and audit metadata |
| Query layer | sqlc + pgx | Type-safe Go queries and explicit SQL migrations |
| Durable workflows | Temporal | Session lifecycle, retries, timers, approvals, cancellation, and recovery |
| Ephemeral state | Redis | Presence, rate limiting, short-lived locks, and resumable WebSocket coordination |
| Event transport | NATS JetStream | Durable runner events and scalable fan-out between the execution and control planes |
| Object storage | S3-compatible storage | Compressed logs, test reports, large diffs, snapshots, and artifacts |
| Authentication | Clerk initially; OIDC/JWT boundary | GitHub sign-in, sessions, invitations; replaceable behind identity interfaces |
| Source control | GitHub App | Least-privilege repository access, webhooks, branches, commits, and pull requests |
| Billing | Stripe | Plans, checkout, subscriptions, invoices, and billing portal |
| Agent execution | Ephemeral Linux microVM or gVisor-isolated container | Runs untrusted repositories and coding-agent processes |
| Container scheduling | Kubernetes Jobs or managed isolated-compute provider | Runner placement, resource quotas, lifecycle, and autoscaling |
| Secrets | GCP Secret Manager with managed encryption and scoped workload identity (ADR-019) | BYOK values outside product storage; delivery not enabled |
| Observability | OpenTelemetry, Prometheus, Grafana, Sentry | Traces, metrics, dashboards, alerts, and application errors |
| Infrastructure | Terraform | Repeatable staging and production infrastructure |
| CI/CD | GitHub Actions | Validation, builds, security scans, migrations, and deployments |

## Deployment Units

1. **Web** — Next.js application; never holds provider or repository credentials.
2. **API** — Go control-plane service; owns synchronous product operations and authorization.
3. **Realtime gateway** — authenticated WebSocket/SSE connections, presence, and event replay. It may begin inside the API binary but must remain a distinct package and deployment configuration.
4. **Workflow workers** — execute Temporal workflow/activity code for session lifecycle and integrations.
5. **Event ingestor** — consumes runner events from NATS JetStream, then validates, deduplicates, sequences and persists them to `session_events`, quarantining what it refuses. Separate from the workflow workers because it scales with runner output rather than with sessions, and because neither may stall the other: a poison event must not hold up workflows, and a Temporal outage must not stop ingestion. Its readiness is PostgreSQL and NATS only.
6. **Runner manager** — provisions, monitors, and terminates isolated runners; it does not make product authorization decisions.
7. **Runner** — per-session ephemeral environment containing a repository checkout, provider adapter, policy-enforcing tool proxy, and event emitter.
8. **Registry proxy** — the second public edge (ADR-016): receives the package-registry requests Vercel's firewall forwards from runner sandboxes, authenticates each by the sandbox OIDC token Vercel signs, records it per session before forwarding it to the registry, and fetches from nothing but its fixed registry list. Separate because it authenticates sandboxes rather than people and fetches from the internet on their behalf; installs depend on it, by design.

9. **Egress proxy** — the third public edge (ADR-017): receives requests Vercel's firewall forwards to a workspace's added hosts, authenticates each by the sandbox OIDC token with an audience bound to the exact host's route, authorizes the host against the runner's own immutable snapshot, and fetches only through the guarded transport — every DNS answer validated, only public numeric addresses dialed, original-host TLS verified, no redirects followed. Separate from the registry proxy because it fetches from admin-chosen hosts rather than a fixed list; added hosts depend on it, by design.

**Hosting direction accepted in ADR-019:** Vercel web/sandboxes, GCP Cloud Run for
stateless HTTP services, and always-on compute for workers/runner manager/NATS.
The secret-access manager identity must not be shared through VM metadata with
less-privileged workloads; use separate instances or proven per-workload identity.
Projects/region/replication, actual IAM, operational HA and deployment spending
remain staging decisions. No cloud deployment is implemented by this selection.

Only the runner manager and workflow workers can provision runners. The browser never connects directly to a runner.

## System Boundaries

- `apps/web` — routes, UI composition, accessibility, client state, and generated API usage
- `services/api` — HTTP API, authentication verification, authorization enforcement, and business modules
- `services/worker` — durable workflow and external-integration activities
- `services/ingestor` — the session event consumer; treats every field of a runner event as an untrusted claim, checked against rows the runner did not supply
- `services/runner-manager` — execution-environment lifecycle and runner health
- `services/runner` — provider adapters and process supervision inside the sandbox
- `services/registry-proxy` — records and forwards runner registry requests; treats every forwarded header as a claim, trusting only what Vercel's signed token proves
- `services/egress-proxy` — forwards runner requests to workspace-added hosts, authorized by the runner's own snapshot and fetched only through `internal/adapters/guardedhttp`; logs no request content
- `internal/domain` — provider-independent entities, value objects, policies, and state transitions
- `internal/application` — use cases and transaction boundaries
- `internal/adapters` — PostgreSQL, Redis, NATS, Temporal, GitHub, Stripe, object storage, and identity implementations
- `contracts/openapi` — canonical public API description
- `contracts/events` — versioned runner and session event schemas
- `db/migrations` — forward and rollback database migrations
- `infra` — Terraform, Kubernetes manifests/Helm, dashboards, and runbooks
- `docs` — product context, architecture decisions, threat model, and operations guidance

Domain code must not import web frameworks, SQL drivers, provider SDKs, queue clients, or cloud SDKs.

## Request and Event Flow

### Synchronous Request

1. The web application sends an authenticated request with a request ID and optional idempotency key.
2. The API verifies the JWT, resolves the workspace membership, and authorizes the exact resource/action pair.
3. Input is validated before business logic runs.
4. The application service executes within an explicit transaction when multiple records must change atomically.
5. An outbox record is written in the same transaction for any asynchronous consequence.
6. The API returns a versioned response envelope with a stable error code when unsuccessful.

### Session Execution

1. `CreateSession` writes the session, policy snapshot, branch intent, and outbox event atomically.
2. An outbox publisher starts a Temporal workflow using the session ID as the workflow ID.
3. The workflow validates quota and GitHub access, creates the branch, and asks the runner manager to provision a sandbox.
4. The runner emits versioned events through NATS JetStream.
5. An event ingestor validates, deduplicates, sequences, and persists durable session events. Sequence is per session and gapless, assigned at acceptance; a transition into a terminal state and an event append are serialised on the session's event counter, so no event lands after a session ends.
6. The realtime gateway fans authorized events to connected clients and supports replay from the last sequence.
7. Large payloads are stored in object storage; events retain immutable references and integrity hashes.
8. Approval waits use durable Temporal signals and timers.
9. Completion activities generate a commit, pull request, summary, usage record, and cleanup request.

## Session State Machine

Allowed top-level states:

- `draft`
- `queued`
- `provisioning`
- `running`
- `waiting_for_input`
- `waiting_for_approval`
- `pausing`
- `paused`
- `resuming`
- `review_ready`
- `finalizing`
- `completed`
- `cancelling`
- `cancelled`
- `failed`
- `expired`

State transitions are enforced by the domain layer with optimistic concurrency. Every transition records the previous state, next state, reason, actor, timestamp, and session version. Terminal states are `completed`, `cancelled`, `failed`, and `expired`; reopening creates a new continuation session rather than mutating terminal history.

## Provider Adapter Contract

The product never calls a provider CLI directly from the control plane. The runner exposes a normalized adapter contract:

```go
type CodingAgent interface {
    Capabilities(ctx context.Context) (Capabilities, error)
    Start(ctx context.Context, req StartRequest) (<-chan ProviderEvent, error)
    SendInstruction(ctx context.Context, req InstructionRequest) error
    Pause(ctx context.Context) error
    Resume(ctx context.Context) error
    Cancel(ctx context.Context) error
    CollectUsage(ctx context.Context) (Usage, error)
    Close(ctx context.Context) error
}
```

Adapters normalize provider output into documented events such as `plan.updated`, `message.created`, `tool.proposed`, `tool.started`, `tool.completed`, `file.changed`, `verification.completed`, `usage.updated`, and `provider.failed`. Provider-specific fields may exist only in a namespaced metadata object.

No feature may assume every provider supports pause, structured tool calls, token accounting, or identical permission semantics. The UI derives available controls from the capability response.

## Tool and Approval Architecture

Agent tools execute through a policy-enforcing proxy rather than unrestricted shell aliases.

Each proposed action contains:

- Stable proposal ID
- Tool and operation name
- Normalized parameters
- Human-readable explanation
- Risk classification
- Repository and session scope
- Policy version
- Content hash
- Expiration time

An approval grants permission only for the exact proposal hash. Parameter changes create a new proposal. Workspace policy can auto-allow low-risk operations, require one approver for high-risk operations, and disable destructive operations. The runner validates the signed approval token immediately before execution.

## Storage Model

### PostgreSQL

Stores users, workspaces, memberships, repositories, tasks, agents, sessions, state transitions, event indexes, messages, approvals, policy snapshots, integration references, usage, subscriptions, idempotency records, outbox records, and audit logs.

Session task title/body is captured atomically at session creation in
`session_input_snapshots`, under the task lock; agent settings remain pinned to
append-only `agent_versions`. Legacy sessions have no reconstructed input and
future real-runtime delivery must refuse them (ADR-018, spec 28). Neither input
text nor provider credentials are carried in workflow/outbox metadata. Credential
ownership is BYOK: workspace-supplied provider API keys, billed directly by the
provider. GCP Secret Manager is selected (ADR-019); authenticated retrieval/delivery
is not implemented. No platform key or local subscription/login fallback.

Spec 30 adds unwired workspace/provider binding metadata: forced tenant RLS,
immutable resource ownership/numeric versions, epoch-fenced owner/admin lifecycle,
transactional audit/idempotency and a required trusted-reference verification port.
Only tests supply fake verification. Safe status excludes restricted references;
no key-value column, production registration path, cloud adapter, credential runner
snapshot or runtime cleanup/delivery exists. Metadata-active is not runtime-enabled,
and metadata-disable is not recall of an exported key.

Spec 31/ADR-020 define the next design boundary, not new implementation:
a protected onboarding approval authority pins exact resource/version creation
identity and tenant assignment independently of labels, plus minimal nonreuse
tombstones surviving workspace deletion. Owner confirmed spec 32 retention/capability direction; concrete ledger/migration
implementation review remains required.
The proposed metadata verifier has no payload capability; a separate manager-only
bounded accessor remains unwired until allocation authorization/delivery gates land.
No approval ledger, GCP SDK or payload accessor is implemented by this proposal.

Spec 32 adds pure exact creation/approval values and a scoped authority reader/
resolver interface, unwired and test-faked only. Result validation/cancellation/
redaction is not ownership proof; the trusted ledger and vendor observation checks
are still absent. Owner confirmed concrete retention and separate capability-role direction after
PR37; ledger/migration implementation and deployment attachment remain unwired.
No existing service factory or provider selection consumes these new contracts.

Every tenant-owned table includes `workspace_id`. Repository functions require workspace scope explicitly; there is no unscoped `GetByID` for tenant data. PostgreSQL row-level security is enabled as defense in depth for high-risk tables, with the application setting the verified tenant context per transaction.

### Object Storage

Stores large or immutable blobs: complete command logs, repository snapshots when required, compressed event payloads, generated patches, test reports, build artifacts, and support bundles. Objects use opaque keys and server-side encryption. Access is through short-lived signed URLs issued only after authorization.

### Redis

Stores presence, connection routing, rate-limit counters, and disposable coordination state. Redis is not the system of record for session state, messages, approvals, or billing.

### NATS JetStream

Carries at-least-once runner events. Consumers must deduplicate by `(session_id, runner_id, event_id)`. NATS retention is operational, not the permanent user-visible history.

### Temporal

Stores durable workflow execution state and timers. Product-readable session state remains in PostgreSQL. Workflow code must be deterministic and versioned using Temporal change/version mechanisms.

## Core Data Model

Primary entities:

- `users`
- `workspaces`
- `workspace_members`
- `workspace_invitations`
- `github_installations`
- `repositories`
- `repository_permissions`
- `tasks`
- `agents`
- `agent_versions`
- `sessions`
- `session_participants`
- `session_state_transitions`
- `session_events`
- `session_messages`
- `action_proposals`
- `approval_decisions`
- `artifacts`
- `pull_requests`
- `policy_sets`
- `policy_versions`
- `usage_ledger`
- `subscriptions`
- `idempotency_keys`
- `outbox_events`
- `audit_events`

Use UUIDv7 identifiers for sortable, globally unique IDs. Mutable resources include an integer `version` for optimistic concurrency. Timestamps are UTC with timezone. Soft deletion is allowed only where recovery or compliance requires it; security logs and usage records are append-only.

## Authentication and Access Model

- Clerk provides initial user authentication and GitHub sign-in.
- The API validates issuer, audience, signature, expiry, and token type locally using cached JWKS.
- Identity-provider IDs are external identifiers and never used as internal primary keys.
- Workspace authorization uses explicit RBAC plus resource attributes.
- Roles are `owner`, `admin`, `developer`, and `viewer`.
- Permissions are action-oriented, such as `session:create`, `session:control`, `action:approve`, `repository:manage`, and `billing:manage`.
- Role checks occur server-side for every request and WebSocket subscription.
- Membership and repository-access changes invalidate active authorization caches and may revoke session control.
- Service identities use workload identity, not shared static API keys.
- Enterprise SAML, SCIM, and domain claims are deferred but supported by the internal identity boundary.

## Multi-Tenancy

- Every request resolves exactly one workspace context before accessing tenant-owned data.
- Cross-workspace joins are prohibited in repository APIs and tested automatically.
- Object-storage keys begin with opaque environment and workspace partitions, but authorization never relies on key shape alone.
- NATS subjects use opaque tenant identifiers and are accessible only to service identities, and to runners holding a credential that may publish their own session's subject and subscribe to their own reply inbox, and nothing else (ADR-014).
- Encryption keys are environment-separated; enterprise customer-managed keys are a future extension.
- Usage, quota, logs, caches, and metrics avoid exposing customer names or source content in labels.

## Runner Security

The execution plane treats the repository, task text, dependencies, generated code, and provider output as untrusted.

- One ephemeral sandbox per session; no sandbox reuse between workspaces
- Read-only base image with ephemeral writable volumes
- Non-root process and dropped Linux capabilities
- Seccomp/AppArmor profile plus gVisor or microVM isolation
- CPU, memory, process, disk, output, and wall-clock limits
- Deny-by-default outbound network policy with domain and protocol allowlists
- No inbound network exposure from the public internet
- Short-lived, task-scoped GitHub credential delivered after provisioning
- Provider credential injected at runtime, never written to the repository volume
- Secret redaction before logs leave the sandbox
- Dependency-cache partitions scoped to trusted dimensions and never shared across tenants if writable
- Automatic teardown on completion, cancellation, timeout, or lost heartbeat
- Image signing, SBOM generation, vulnerability scanning, and admission enforcement

Production must not use a plain privileged Docker socket or mount the host filesystem into customer runners.

**Decided in ADR-013** (`docs/adr/0013-runner-execution-boundary.md`):

- **Isolation** runs on a managed microVM or gVisor provider behind a `RunnerBackend` port, accepted only against the ADR's checklist — deny-by-default egress foremost. Kubernetes with gVisor is the scale-up path. Local development uses a hardened-Docker backend that is **not a security boundary** and that the runner manager refuses to start in staging or production.
- **Source is not retained.** The sandbox, checkout and writable volumes are destroyed at teardown; only derived artifacts remain. A continuation re-clones at `sessions.branch_sha` and applies the stored patch. No persistent dependency cache in the MVP.
- **Egress** is HTTPS on 443 through a hostname-enforcing egress proxy. The default allowlist is GitHub, the configured model provider, and the npm, PyPI, Go, crates.io, RubyGems and Maven Central registries. Metadata endpoints, private and link-local ranges, internal services, raw IPs and other ports are always refused. Workspace admins may add hostnames for their own sessions — audited and capped.
- **The provider is Vercel Sandbox** (Firecracker microVMs), selected against ADR-013's checklist on 2026-09-29; its firewall is the hostname-enforcing proxy, and every sandbox is created non-persistent, because Vercel's default snapshots the filesystem on stop.

**Decided in ADR-015** (`docs/adr/0015-runner-event-ingress.md`):

- **Runners reach NATS only through a public WebSocket listener**, TLS on 443 at one dedicated hostname — the one Weave-operated destination on every sandbox's allowlist. The standard client port is never exposed. Runner credentials may connect only over WebSocket and service credentials only over the standard port, so neither is usable on the other's listener.

**Decided in ADR-016** (`docs/adr/0016-registry-request-logging.md`):

- **Every registry request is recorded before it is forwarded.** Default registry hosts are forwarded by Vercel's firewall to the registry proxy, with no `match` so no request escapes it; the proxy authenticates each by Vercel's sandbox OIDC token (issuer, project, and an audience bound to the exact route), resolves the runner, records method, host and path per session, then fetches — and refuses rather than forwards when it cannot record.

**Decided in ADR-017 — implemented in M5.4d.1–3a:** workspace-added
destinations are forwarded, with no match filter, to a separate authenticated
egress proxy — never plain firewall rules, never the fixed-list registry proxy.
The proxy authorizes each request against the runner's immutable snapshot
(written by a database trigger at runner creation), resolves and rejects
forbidden IPv4/IPv6 answers on each new connection, and dials only validated
numeric addresses with original-host TLS verification. No redirect following or
environment proxy. Provisioning fails closed on a missing snapshot, a missing
proxy URL, or a host that has since become reserved. Existing default policy is
unchanged. The edge's plaintext handling and availability are Weave's
responsibility. The admin screen follows in M5.4d.3b.

## Reliability and Consistency

- PostgreSQL is the source of truth for product state.
- Transactional outbox prevents lost asynchronous work.
- API mutations accept idempotency keys where retries may duplicate side effects.
- External webhook events are signature-verified, deduplicated, and retained with delivery metadata.
- Runner events are at-least-once and deduplicated before sequencing.
- Side effects such as branch, commit, pull-request, and billing updates are recorded with provider identifiers.
- Temporal activities use bounded retries, exponential backoff, timeouts, and non-retryable error classes.
- Heartbeats detect stalled activities and runners.
- Compensating cleanup handles partially provisioned resources.
- Schema changes follow expand/migrate/contract deployment sequencing.

## Performance and Scale Targets

Initial service-level objectives:

- API availability: 99.9% monthly after general availability
- Read API p95 latency: under 300 ms excluding third-party calls
- Mutation API p95 latency: under 500 ms excluding third-party calls
- Session event propagation p95: under 2 seconds
- Reconnect and replay for 10,000 recent events: under 5 seconds
- No acknowledged instruction or approval lost during a single-service restart
- Runner provisioning p95: under 60 seconds with warm capacity

Scale horizontally by API, gateway, workflow-worker, event-ingestor, and runner-manager role. PostgreSQL uses connection pooling and read replicas only when measurements justify them. Large session histories are paginated and eventually archived to object storage while retaining indexed metadata.

## Observability

- W3C trace context propagates from browser request to API, workflow, runner manager, and runner events.
- Logs are structured and include environment, service, request ID, workspace ID hash, session ID, and error code.
- Source code, prompts, secrets, and terminal output are excluded from default infrastructure logs.
- Metrics cover latency, errors, saturation, queue lag, workflow age, runner provisioning, runner heartbeat, event delay, provider failures, approvals, and cost.
- Alerts exist for stuck workflows, high failure rates, event-ingestion lag, credential errors, capacity exhaustion, and cleanup failures.
- Sentry captures application errors with tenant-safe context and documented data scrubbing.

## Backup, Recovery, and Retention

- PostgreSQL point-in-time recovery with daily restore verification
- Object-storage versioning and lifecycle policies
- Infrastructure state stored remotely with locking and backup
- Recovery point objective: 15 minutes for transactional metadata
- Recovery time objective: 4 hours for the initial paid product
- Session event retention configurable by plan; audit events retained according to policy
- Documented regional restoration and provider-outage runbooks
- Quarterly recovery exercise after general availability

## Environments and Delivery

- Local, test, staging, and production use separate accounts, credentials, databases, queues, buckets, and encryption keys.
- Pull requests run unit tests, integration tests, linters, type checks, secret scans, dependency scans, and container scans.
- Main-branch builds produce signed, immutable images.
- Staging receives automatic deployment after validation.
- Production deployment is promoted from the tested artifact with approval.
- Database migrations run as a controlled job before compatible application rollout.
- Rollback uses prior immutable images; irreversible migrations require an approved recovery plan.

## Invariants

1. The browser never receives GitHub App private keys, provider keys, runner credentials, or direct runner access.
2. Control-plane request handlers never execute agent work or other long-lived jobs inline.
3. Every tenant-owned data access is scoped by a verified workspace ID and authorization decision.
4. A runner can access only the repository, branch, credentials, network destinations, and resources granted to its session.
5. Every external side effect is idempotent, auditable, and attributable to a human, agent, or service identity.
6. An approval applies only to the immutable action proposal it references and cannot be reused across sessions.
7. PostgreSQL is the source of truth for product-visible state; caches and streams cannot silently override it.
8. Provider-specific behavior remains behind adapters and capability negotiation.
9. Session events are append-only and ordered; corrections are new events, not history rewrites.
10. Terminal session history is immutable; continuation creates a linked new session.
11. Destructive tools are disabled by default and protected branches never receive direct agent writes.
12. No production release proceeds without passing tenant-isolation, authorization, migration, and runner-security checks.

## Architecture Decision Records Required

Create an ADR before changing any of these decisions:

- Control-plane modular monolith boundary
- Temporal as the durable workflow engine
- NATS JetStream as execution-event transport
- PostgreSQL tenancy and row-level-security strategy
- Runner isolation technology
- GitHub App credential model
- Approval-token format and trust boundary
- Provider-adapter contract
- Audit-retention policy

