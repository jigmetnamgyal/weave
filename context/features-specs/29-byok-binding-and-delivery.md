# M6.1b.2b–d — BYOK binding and delivery plan

**Status: plan; slice (b) implemented under verification/review in
[spec 30](30-provider-credential-binding-metadata.md).** BYOK and GCP direction
accepted (ADR-018/019). No production registration path, cloud access, runner
credential snapshot, secret delivery or real-provider activation is enabled.

## Outcome

An authorized workspace can bind its own provider API key without placing key
values in product storage/history. A trusted allocator can resolve exactly the
version bound to its runner, or refuse safely. Fake sessions remain key-free and
real Claude selection stays disabled until every activation gate passes.

## Split into independently verifiable slices

1. **M6.1b.2b — binding metadata:** domain/service/sqlc/PostgreSQL lifecycle, tenant
   RLS, audit and idempotency. Internal registration via trusted onboarding only;
   deterministic fake verification of secret reference ownership. No key bytes,
   production wiring, new public API or image change. Implemented under review in spec 30.
2. **M6.1b.2c — GCP reference verification/retrieval:** approved pinned SDK/license,
   exact project/resource/version validation, scoped workload identity, bounded
   response, payload integrity, safe errors and fake contract tests. No implicit
   credential discovery in CI/dev, cloud resource creation or paid model call.
   Live identity/secret access needs separately approved synthetic sandbox resources.
3. **M6.1b.2d — authenticated runner handoff:** versioned bounded input/secret
   protocol, allocation snapshots, delivery authorization and idempotent receiver,
   isolated backend capability acceptance. Compatibility review required before
   changing RunnerBackend, StartRequest or the existing secret re-exec. No secret
   or input body in Temporal/NATS/outbox, no adapter activation as a shortcut.

Slice (b) does not allocate credential snapshots or deliver/revoke runner keys:
there are no real key-bearing runners yet. Durable affected-runner cleanup,
receiver receipts and allocation fencing must land in (d) before activation;
metadata-only disable must not be described as implemented runtime revocation.

Agent instructions require a distinct versioned profile field/contract later.
Never synthesize them from tool-policy JSON or repository files. Task title/body
is assembled only from spec 28 snapshots; legacy missing input refuses. Assembly
must fit the supervisor's encoded 128-KiB limit without truncation or shell use.

## Planned persistence contract — not a migration

- `workspace_provider_credentials`: one workspace/provider identity, lifecycle
  (`pending`, `active`, `revoked`), current approved version and monotonically
  increasing epoch. UUIDv7 identity; one binding per pair. Initial provider is
  Claude Code; fake never has/requires a binding, Codex support is deferred.
- `provider_credential_versions`: immutable registration records with composite
  workspace/binding identity, credential kind `api_key`, canonical environment
  project + opaque secret resource + numeric version, trusted ownership evidence
  and actor/time attribution. No secret value or secret-derived fingerprint.
- `runner_provider_credential_snapshots`: immutable runner/session/workspace FK,
  binding/version/epoch selection made atomically at allocation. Missing data
  refuses real activation; no backfill from mutable current bindings.
- Forced RLS, explicit workspace-scoped reads, validated composite FKs, mutation
  guards and no generic client-controlled snapshot INSERT. Reference/version
  reuse across tenants must be refused. Resource names/ownership evidence are
  restricted internal data; no public route/log exposes them.

These table names/states are proposals until the next slice defines migration and
ports. Append-only evidence does not authorize use; current binding state does.

## Registration, rotation and disable semantics

- Owner/admin through existing `workspace:manage`, reauthorized transactionally;
  developer/viewer/removed/foreign/unauthenticated actors denied. No new permission
  inferred from provider capabilities or current-agent settings.
- Trusted onboarding creates a tenant-designated Secret Manager resource out of
  band. A client-supplied path, UUID or labels alone cannot prove ownership.
  Use an explicit verification port; unwired verification fails closed. A fake
  is for tests only, never a production ownership attestation.
- Verify external metadata outside the short write transaction. Then lock/recheck
  workspace authorization, expected epoch and registration evidence before committing
  approved immutable reference, active pointer, audit and idempotent completion.
  External secret versions are immutable while present; no resource-ID reuse or
  mutable alias is allowed. Destruction/disable means unavailable, not version fallback.
- Rotation registers a new immutable version and advances epoch. It affects future
  allocations; undelivered old snapshots fail epoch checks, never silently switch
  to the new key. Already-started runners retain their key unless explicitly
  cancelled/revoked. Provider-side old-key rotation remains customer-controlled.
- Disable/revoke advances epoch and blocks new authorization. Persist a durable
  cleanup intent in the same transaction for all live runners referencing that
  binding. Cleanup must be idempotent, observable and retried after outages.
  Cancellation/revocation scope covers outstanding authorized deliveries as well
  as runners already marked running. Do not delete history to express revocation.

## Planned retrieval and delivery sequence

1. Authorize the manager's service identity and derive workspace from trusted
   session/runner records, not an inbound claim. Validate exact runner is live,
   session is in the allowed startup state, input exists, active binding/version
   and epoch match the immutable allocation snapshot. Write bounded attempt/lease
   metadata only. Reject before any external fetch on failure.
2. Access only the explicit registered numeric Secret Manager version through the
   manager identity. Fixed vendor endpoint/project configuration, bounded deadline
   and key size; validate response identity/integrity. No alternate-version or
   ambient/global-key fallback; no live model call to validate a key.
3. Recheck session, runner, binding and epoch in a short transaction. Commit a
   single allocation/attempt delivery authorization. If stale, discard retrieved
   bytes and refuse; never return key values from the activity or persist them.
4. Deliver inside that trusted activity/process over the backend's authenticated
   management channel to a trusted receiver outside checkout. Separate structured
   task input and secret data; no shell/argv/URL query, persistent vendor config,
   repository file or durable queue. The exact Vercel transport is a capability
   gate, **not assumed supported**. No custom plaintext file/NATS workaround.
5. Receiver validates allocation, grant and attempt identity; repeat delivery of
   the same attempt must not start another CLI. An ambiguous backend timeout
   requires receipt/status reconciliation or teardown, never blind resend/new
   attempt against an unknown-running receiver. Late acknowledgement rechecks
   current state and cannot revive/complete a revoked or cancelled allocation.

Checks at steps 1/3 define authorization points, not atomicity with an external
network write. Cancellation after step 3 can race an in-flight handoff. Revoke
blocks later grants and cleanup converges; it cannot erase already-exported keys.
Do not claim exactly-once delivery or instant revocation without demonstrated
receiver protocol/provider semantics. Key lifetime in Go memory is minimized,
not claimed cryptographically zeroized. Same-UID tools remain an M7 threat gate.

## Acceptance tests to implement per slice

- Success, permission matrix, missing/disabled/revoked binding, wrong tenant and
  explicit/no tenant RLS; forged reference/ownership evidence, project or provider
  mismatch and shared-reference refusal.
- Atomic audit/idempotency failures and replay; concurrent registration/rotation,
  epoch conflict and membership removal between verification and commit.
- No mutable alias, old epoch/new version mixing or legacy runner fallback;
  cancellation before fetch, during fetch, before authorization and during delivery.
- Secret denial/outage/timeout, oversize/integrity refusal, safe error categories;
  negative fixtures with secret markers in SDK errors/debug metadata and every
  formatter/serialization/log/event/history output. No raw SDK diagnostics.
- Delivery duplicate/unknown receipt, lost acknowledgements, late ack after revoke,
  cancellation and teardown retry. Confirm no second process and no leaked checkout,
  argv, image/snapshot, workflow result, broker message or infrastructure log.
- Assert denied cases make zero secret-store/backend calls. Deliberate mutations
  must catch removed scope, epoch/state checks and redaction/isolation safeguards.
- Unit/race, real app-role PostgreSQL integration, migration recovery, full local
  integration on isolated DB/test-scoped queues; contract checks for changed ports.

## Remaining gates

Choose environment/project/region, replication policy and IAM/deployment settings
before actual resources; verify current vendor SDK/API behavior. Staging cost/terms
approval, Vercel Pro/survival gates, CLI artifact/terms/isolation, provider egress,
secret redaction and explicitly opt-in bounded paid acceptance remain. This design
is not verification of a real Secret Manager or Claude session.

## Next

Review M6.1b.2b metadata implementation/security evidence in spec 30. Then separately
implement trusted GCP reference verification/retrieval, followed by capability-tested
authenticated delivery and cleanup. No cloud provisioning/key access/paid calls are
approved by this metadata slice. Keep fake sessions working throughout.
