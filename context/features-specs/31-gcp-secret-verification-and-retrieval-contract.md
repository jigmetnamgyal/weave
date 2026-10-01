# M6.1b.2c.0 — GCP verification/retrieval contract

**Status: planning merged in PR #36 (1501e02), not implementation or live acceptance.**
Follows merged spec 30, plan spec 29 and accepted ADR-018/019. ADR-020 proposes
additional trusted approval/creation evidence; its retention/grants require review.

## Outcome, scope and non-scope

Define a reviewable boundary for proving a registered reference's ownership and
resource incarnation before bounded key access. This unit changes documentation
only: contracts, authority, failures, acceptance matrix and staged implementation.

No compiled accessor, SDK installation, dependency/lockfile, migration, approval
writer, public route, service composition, payload fetch or cloud resource is
included. No changes to `ProviderReferenceVerifier`, `RunnerBackend`, runner
`StartRequest`, existing secret re-exec, egress policy or real provider selection.
Fake sessions remain key-free; real Claude remains disabled.

## Trust root: approval is not a label

A protected onboarding authority must record, from approved operations:

- Environment, numeric project identity, API resource family and canonical opaque
  secret UUID/name; trusted workspace/provider and credential kind `api_key`.
- Exact secret creation identity and explicit positive numeric version, with exact
  version creation identity. Use vendor timestamp seconds/nanoseconds without
  truncation if current API validation establishes them as reliable evidence.
- Unique approval UUID, schema version and trusted actor/time attribution; an
  append-only withdrawal decision determines whether it is currently usable.
- Approved project canonical naming, replication policy and designated workload
  identities. These are trusted deployment/onboarding facts, not request options.

Reserve the opaque resource identifier in the protected nonreuse authority before
any external creation. Failed/abandoned/ambiguous creation never frees that name;
reconcile the exact intent or retire it, not recreate/reassign it. Cloud creation
and PostgreSQL persistence are not a single transaction.

The producer must establish workspace assignment independently of secret labels
or requester claims, reconcile cloud creation/read-back with the approved intent,
and atomically persist the scoped immutable evidence. Ambiguous onboarding cannot
publish an approval. Version rotation needs its own exact version approval.
Unproven pre-existing/adopted resources are refused, not auto-approved from IAM
success. A provider API key's actual account/billing/validity is not established by
cloud metadata; no paid provider request validates it in this slice.

ADR-020 proposes a protected PostgreSQL ledger and minimal permanent nonreuse
resource tombstones surviving workspace deletion. These are **not tables created
by migration 00017**. Concrete retention, privileged grants, schema and approval
writer need approval/review before implementation. UUID-shaped naming alone never
proves nonreuse; exact creation evidence and nonreuse authority work together.

Current `provider_credential_versions.verification_id` must resolve to the exact
approval/workspace/provider/reference/incarnation/kind. A random test witness or
unknown UUID is refused in production. Do not reconstruct old approvals from live
labels or turn synthetic rows into verified production bindings.

## Proposed consumer ports (illustrative, not compiled Go)

Keep the existing metadata-only port unchanged:

```go
// Existing application.ProviderReferenceVerifier, not a payload accessor.
Verify(ctx context.Context, workspace uuid.UUID, provider domain.Provider,
    ref domain.ProviderSecretReference) (domain.VerifiedProviderReference, error)
```

Future application consumers define narrow ports; these sketches are not new
public APIs or bearer capabilities:

```go
// Scope derived from trusted records, never an inbound ownership assertion.
LookupApproval(ctx context.Context, workspace uuid.UUID, provider domain.Provider,
    ref domain.ProviderSecretReference) (ApprovedSecretVersion, error)

// Exact durable approval, including creation evidence; no arbitrary UUID lookup.
LookupSelectedApproval(ctx context.Context, selection SelectedCredential) (
    ApprovedSecretVersion, error)

// Manager-internal, bounded borrow; no key returned as a durable result.
WithSecret(ctx context.Context, selection SelectedCredential,
    consume func(context.Context, SecretBorrow) error) error
```

`ApprovedSecretVersion` is restricted evidence with the above fields.
`SelectedCredential` identifies the exact immutable product version, approval UUID,
workspace/provider/reference and, once slice (d) exists, allocation/binding epoch.
Construct it from trusted records, not client JSON. The ledger independently
checks exact scope and usable approval on every call. No missing-field/default
version or current-binding fallback. Typed Go fields cannot prove caller authority.

`SecretBorrow` would hold private bytes, redact every fmt verb, refuse JSON/text/
binary durable serialization and expose bytes only to the trusted in-process
consumer during the callback. It is not returned by an activity. Clear owned
buffers best-effort on all exits; a callback can copy bytes, Go/SDK may copy them,
and panic/crash may retain memory. No secure-zeroization or enforced nonretention
claim. Minimize lifetime; callbacks must cooperate with cancellation. Do not start
an unbounded goroutine to pretend to forcibly stop a callback.

No manager selection/allocation authorization port exists yet. Slice (c)'s accessor
can prove approved cloud reference validity, **not runtime permission**. Production
composition remains blocked until slice (d) supplies current runner/session/binding/
epoch checks before fetch and again before authorizing delivery, plus cleanup.

## Proposed adapter construction and identity contract

- Explicit approved config: environment/project, canonical name mapping, API family,
  fixed endpoint/TLS, resource namespace, expected workload identity, concurrency,
  deadline/response limits. Required client/authority/identity dependencies,
  including typed-nil implementations, fail construction. No lazy missing-config
  fallback and no network/token discovery from test constructors.
- Supply an explicitly selected native workload identity provider at composition.
  Nil credential options must not make the SDK choose ADC/operator login, env key
  files, a default project or local Claude credentials. No service-account JSON,
  broad project-wide payload access or inherited environment proxy.
- A nonnil injected client/token source alone is not evidence of the principal.
  Approved native identity binding and denial tests must establish the expected
  service account and resource scope before live enablement. API and metadata
  verifier identities have no payload-read grant; manager identity is isolated
  from less-privileged VM workloads (ADR-019).
- Candidate SDK: official `cloud.google.com/go/secretmanager/apiv1`. No version,
  transitive dependency/license/security review or installed API behavior is
  approved/verified here. Record those before adding a dependency.
- Candidate MVP resource family is global Secret Manager API resources. Regional
  secrets use a different naming/config contract and are excluded unless separately
  reviewed. Global API selection does not choose replication regions or establish
  residency. Current `ProviderSecretReference` has no location field; never quietly
  accept regional paths or overload its project/UUID fields.
- Namespace proposal: fixed `weave-provider-<canonical-lowercase-secret-uuid>` under
  the approved project, then `/versions/<positive-decimal-version>`. No request
  URL, host, project alias, prefix, transport option or `latest` alias. Actual
  vendor response canonicalization is an SDK acceptance gate; allow only explicitly
  approved project-number/ID equivalence, not suffix/substring matching.

## Metadata verification sequence (zero payload access)

1. Existing service checks current owner/admin authority, environment/project,
   provider, syntax and expected epoch (spec 30).
2. Verifier looks up exact usable trusted approval; missing/wrong scope/withdrawn
   evidence returns fixed rejection **before any cloud call**.
3. Under the existing cooperative five-second budget, fetch only approved secret
   and numeric-version metadata. Compare exact names/project identity, secret and
   version creation observations, kind/replication requirements and enabled state.
   Mutable labels/etags are inventory/change hints, not authority or incarnation.
4. Return the exact approved verification UUID and matching scoped reference.
   Existing service then rechecks actor/epoch and commits metadata atomically.

Metadata availability is not proof that payload access will succeed later. A cloud
resource can change after verification; subsequent retrieval revalidates identity
and availability. No cloud read is atomic with PostgreSQL or with delivery.

## Bounded retrieval sequence (manager-only, unwired)

1. Caller supplies a trusted selected credential and has already passed slice (d)
   authorization. Check context and exact current approval/scope before cloud I/O.
2. Recheck approved secret/version metadata and incarnation, then access **only**
   that numeric version. No list/search, alias resolution or another-version retry.
3. Require matching response resource identity under the approved canonical project
   mapping; require present/in-range CRC32C and verify Castagnoli checksum of exact
   payload bytes. CRC is transport integrity, not ownership or authenticity; TLS,
   identity, approval and creation continuity remain separate requirements.
4. Validate key bytes against current supervisor compatibility: nonempty, at most
   **512 bytes**, no NUL/CR/LF. No prefix heuristic, trimming, truncation or model
   request. Preserve bytes exactly. If future requirements exceed this, separately
   review the supervisor contract rather than widening it silently.
5. Recheck context/approval withdrawal before invoking the consumer. Slice (d)'s
   consumer must reauthorize current allocation/session/binding epoch **before any
   backend write**. On failure/cancellation, do not deliver; discard owned buffers.

Proposed limits: five-second end-to-end accessor budget, no automatic SDK retries,
16-KiB RPC/HTTP decoded response cap enforced before full allocation, and at most
8 in-flight manager accesses with context-bounded waiting. These are design values,
not measured SDK guarantees. Metadata labels may make a response exceed the cap;
refuse rather than silently trim. Implementation must demonstrate transport-level
bounds including SDK retries, backoff, token acquisition and callback cooperation.
No key cache, disk spool or background prefetch. Cloud change between observations
can still race access; a post-fetch metadata check is not distributed atomicity.

## Failures, diagnostics and observability

Use fixed categories: configuration, reference/approval rejected, cloud unavailable,
access denied/unavailable version, response identity/integrity refused, payload
refused, cancelled/deadline, consumer/delivery refused. Map SDK and callback errors
at the boundary without wrapping raw messages, URLs, request/response objects,
headers, trailers, status details or context causes. Keep safe cancellation errors.
No server error body or payload-derived hash in logs, caches, audit or tracing.

Metrics use bounded operation/category/provider labels only, no resource, project,
key hash or SDK detail. Traces record safe operation/duration/category; audit uses
internal binding/version/attempt IDs only. Disable SDK request/body/debug logging;
inspect native infrastructure audit behavior before live tests. Correlation IDs
must not be synthesized from keys. Authorization/withdrawal failures do not retry.
Caller-directed outage retries repeat exact selection and all authorization checks,
never switch versions. Metadata disable and approval withdrawal do not erase keys
already exported; provider-side revocation and durable teardown remain required.

## Acceptance and implementation sequence

**This design unit:** inspect existing port/schema/supervisor bounds, check links/
Markdown/diff, review ADR-020 and spec 31. No runtime or vendor behavior claimed.

**Next bounded unit c.1:** approve authority/retention design; implement protected
approval metadata and exact scoped lookup, with explicit constructors, synthetic
fixtures and isolated real app-role tests. No GCP SDK or key access. Migration must
preserve spec 30 binary compatibility, reject unknown synthetic approvals for real
use, document withdrawal projections and recover without reviving old approvals.

**Then c.2:** current official SDK/version/license/IAM documentation review; pinned
unwired adapter and deterministic vendor fake tests. Test:

- Wrong/no scope, unknown/withdrawn approval, forged timestamp/kind/project/provider,
  recreated resource/version and unsupported regional family; zero vendor calls
  when local authority fails and zero access calls on metadata-only verification.
- Wrong response identity, alias/version confusion, disabled/destroyed/missing,
  denied/outage, malformed timestamps/CRC, absent CRC including valid zero CRC,
  mismatched checksum, zero/513-byte payload, NUL/CR/LF and no normalization.
- Cancellation while acquiring capacity/identity/metadata/access/consumer, late
  success after deadline, callback failures, resource withdrawal during I/O, and
  exact-selection/no-fallback retry behavior. No permit/buffer leaks after failure.
- Synthetic secret markers in SDK/callback errors, structured debug fields, fmt/
  serializers/logs/traces/metrics/cache/audit/activity results. Check every error
  category and denied-call counter; mutate ownership/incarnation/name/CRC/limit/
  redaction checks and report any surviving mutations or unrelated failures.

**Then c.3, separately approved live synthetic acceptance:** exact project,
environment, resource family/replication region, identities/IAM and test spend;
create only approved disposable resources, observe metadata/integrity behavior,
verify wrong identities/version states denied, clean up and preserve nonreuse
records. No customer/operator key discovery or paid model call. A live synthetic
secret test is not live Claude acceptance.

Slice (d) remains separate: allocation snapshots/fences, authenticated receiver,
backend capability evidence, ambiguous receipt reconciliation and durable cleanup.
It must land before production access/handoff/Claude activation.
