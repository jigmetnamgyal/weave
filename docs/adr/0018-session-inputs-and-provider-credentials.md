# ADR-018: Session input capture and proposed provider credential delivery

- Status: Input capture merged in PR #34 (2f50688); BYOK and GCP Secret Manager direction accepted by owner (ADR-019); delivery implementation pending
- Date: 2026-10-01

## Context

Tasks are editable after a session is queued. Agent settings are already
append-only versions. Rereading the task at provisioning would change the work a
session executes, while copying agent settings introduces a second source of
truth. Claude's process primitive accepts a caller key but does not establish
its ownership, retrieve it or authorize paid execution.

## Input decision

Capture title/body under the task lock at session INSERT. The session, snapshot,
participant, audit, idempotency response and outbox commit or rollback together.
Keep the immutable agent-version pin; freeze session task/version/repository and
branch intent against retargeting. No mutable agent-name/current-version lookup
is part of input assembly. Current agent versions contain provider/model,
capabilities and tool policy, not instructions. Repository AGENTS/CLAUDE files are
not a substitute for a missing versioned instruction field.

Store task text as tenant-scoped PostgreSQL product data, protected with forced
RLS, SELECT-only app grants and mutation triggers; never infrastructure logs.
There is no historical backfill from current tasks. Missing input is a stable
refusal, not a fallback. Old fake sessions remain executable because this slice
does not yet require inputs in provisioning. Future real adapters require them.

## Credential ownership decision — BYOK

The owner selected BYOK: each workspace supplies a provider API key from its own
provider account and pays the provider directly. Weave does not supply a shared
platform key, resell model usage or assume responsibility for provider charges.
This is an ownership decision, not permission to invoke a model or access a key.

Workspace owners/admins manage the Weave credential binding; the customer retains
provider-account billing and provider-side key rotation/revocation authority.
Prefer a dedicated key per workspace/provider. Claude subscription/login tokens
are not API keys and will not be discovered or reused. Initial secret onboarding
remains out-of-band into the chosen managed secret store; no key-entry/readback
UI or credential ingestion endpoint is authorized or implemented by this decision.

Weave must disable/fence a revoked binding and terminate affected runners;
provider-side revocation is also needed to invalidate an already-delivered API
key. Keys remain long-lived provider capabilities, not session-scoped tokens.

## Credential proposal — not enabled

Required isolation rules for workspace-owned BYOK credentials:

- One explicitly configured binding to **workspace + provider**. No global-key
  fallback, local Claude login discovery, subscription token reuse or silently
  borrowing the operator's API project. No managed credential is added to input snapshots. User-authored text can
  itself contain secrets and is never safe default telemetry.
- Store only credential binding/version and managed-secret reference in product
  metadata. Secret values live in an approved managed secret store with KMS;
  retrieval uses runner-manager workload identity and least-privilege policy.
  Do not implement bespoke crypto or persist keys in PostgreSQL, Temporal
  histories, outbox/NATS, idempotent response caches, events or browser output.
- Authorize live workspace/provider binding, session state and exact runner before
  retrieving. Capture a binding/version reference for allocation; cancellation
  or replacement fences delivery. A retry must not mix an old reference with a
  new secret. Revocation disables new delivery and tears down affected runners;
  an already-delivered long-lived API key cannot be retroactively withdrawn from
  process memory. Provider-side key revocation is a distinct operator action.
- Deliver after provisioning over the authenticated backend secret channel,
  separately from untrusted input. No key in argv, repository volume, persistent
  image/snapshot or forwarding rule. Audit IDs/outcomes only, never secret values,
  secret-bearing URLs or upstream diagnostics. Validate backend errors are safe.
- The API key is a long-lived provider capability, **not** a session-scoped token.
  The sandbox can exfiltrate it to permitted provider destinations; process
  environment isolation alone cannot protect it from arbitrary same-UID tools.
  Tool-enabled activation needs a separate threat-model/containment review (M7),
  redaction before events leave, provider egress and opt-in spend limits.

## Accepted store direction and remaining gates

GCP Secret Manager with scoped workload identities is accepted (ADR-019).
Cloud Run serves stateless HTTP workloads; privileged always-on manager compute
must not share metadata credentials with workers/NATS. No cloud resource is
provisioned or key accessed by this decision.

Environment/project/region, replication, concrete IAM and deployment spending
remain to be configured/approved. Rotation/revocation implementation, budget
enforcement and explicitly authorized sandbox acceptance spending remain gates.
Spec 29 splits binding metadata, GCP retrieval and backend handoff; in-flight
revocation is convergent cleanup, not a promise of instant distributed erasure.

## Threat model and alternatives

The attacker controls task text and repository contents, and may edit a task
while creation/provisioning races. Atomic capture avoids mutable-input substitution;
composite tenant FKs and forced RLS prevent cross-workspace joins. App INSERT on
snapshot rows is withheld; only the narrow trigger can capture a session's task.
A privileged DB administrator can alter guards/data and is outside this boundary.

Rejected: copying today's task into old history; embedding inputs or credentials
in durable workflow arguments; inheriting operator environment/login; unrestricted
provider tools; treating API keys as ephemeral because the sandbox is ephemeral.

## Consequences

This slice establishes reproducible persisted inputs, not delivery or a working
Claude session. Agent instructions, encoded size enforcement, authenticated
handoff, managed secrets, paid execution, image/terms/isolation and provider
network acceptance remain explicit follow-up gates.
