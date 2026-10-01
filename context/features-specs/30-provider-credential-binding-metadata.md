# M6.1b.2b — Provider credential binding metadata

**Status: merged in PR #35 (047b039).** Implements only slice (b)
of [spec 29](29-byok-binding-and-delivery.md), under ADR-018/019. There is no
production composition, public route, key ingestion/readback, cloud adapter,
runner credential snapshot, delivery, cleanup job or real Claude activation.

## Scope and trust boundary

- Internal application service registers approved Claude API-key reference
  metadata, rotates it, reads status and disables the binding. Only current
  owners/admins (`workspace:manage`) can use these operations; system actors are
  refused. Cached membership alone is not authority.
- Explicit environment/project config and a `ProviderReferenceVerifier` are
  required. Nil/unwired dependencies fail construction. No production fake or
  ambient credential discovery exists. Test fakes **do not prove cloud ownership**.
- Before registration, authorize from current membership and reject stale intent;
  verify outside the transaction under a cooperative five-second timeout. Check
  workspace/provider/exact reference and nonnil verification identity, cancellation
  and deadline before persistence. Raw verifier diagnostics never escape.
- Naming/labels/project membership are not proof. The future verifier must consult
  trusted approved ownership and creation identity, reject resource-ID reuse and
  verify the explicit version/API-key kind. Numeric version/UUID syntax checks
  here are not that verification. Current fake evidence is not live acceptance.
- No key-value parameter or storage column exists. References contain environment,
  numeric project number, opaque secret UUID and positive numeric version. No
  URL, arbitrary path, alias or `latest`. Canonical cloud namespace resolution
  and concrete evidence/creation-identity verification belong to slice (c).

## Domain and persistence

Migration `00017_provider_credential_bindings.sql` adds:

1. `workspace_provider_credentials`: one binding per workspace/provider, immutable
   identity/creator, pending/active/revoked status, monotonic epoch, current version.
2. `provider_credential_resources`: immutable resource ownership. Global unique
   `(project_number, secret_id)` prevents another workspace/binding using the
   same resource at *any* version; ownership persists through rotation/disable.
3. `provider_credential_versions`: append-only numeric version, resource/binding/
   workspace composite FK, registration epoch, API-key kind, unique trusted
   verification identity and actor. No raw labels or provider diagnostic evidence.

Every table has enabled **forced** workspace RLS. Application grants permit
binding SELECT/INSERT/UPDATE and resource/version SELECT/INSERT, not deletion,
TRUNCATE or history edits. Explicit workspace query predicates supplement RLS.
Composite FKs fence cross-workspace/current-version and resource retargeting.
Triggers also guard owner-bypass edits, identity changes, epoch skips, reusing a
current version and illegal state transitions. Direct binding/history deletion
is refused; workspace-parent deletion can cascade. Resource ownership rows live
until workspace deletion; future trusted approvals must enforce nonreuse after
product deletion too. This database is not a lifetime cloud creation registry.

New registration temporarily inserts pending/epoch zero inside one transaction;
only complete registration commits active/epoch one. Verification failure does
not leave a pending row. Each rotation/reactivation requires a **new** immutable
version registered for the next epoch; previously registered resource versions
cannot be reused. Disable retains the current pointer/history and increments the
epoch once; repeated disable is an explicit already-disabled error. Stale expected
epochs never automatically borrow current state/retry with another version.

"Active" means approved **metadata**, not a healthy key, a runnable Claude profile
or a cloud/runtime grant. Disable is **not runtime revocation**: no durable runner
cleanup intent or allocation fence is produced yet. Those must land before real
activation; exported long-lived keys still require provider-side revocation.

## Transactions, audit and retries

Store mutations use the existing tenant transaction and workspace lock shared
with membership changes. Recheck current actor and expected epoch at commit,
then persist resource/version, current pointer, safe audit and optional idempotent
completion together. Failure rolls everything back and returns a zero result.

Audit actions are `workspace.provider_credential.registered` and
`workspace.provider_credential.disabled`, with binding/workspace/actor, provider,
state, epoch and internal version UUID only. Status DTOs/response callbacks expose
no project/resource/evidence or key. Reference/evidence types redact all `fmt`
verbs and refuse JSON marshaling. Restricted generated query rows remain internal
storage data and must never be logged/serialized. Unknown PostgreSQL/renderer and
verifier failures map to fixed safe categories without wrapping diagnostics.

`CredentialCompletion` uses the existing claimant fence and validates workspace,
actor and fixed internal operation (`registerProviderCredential` or
`disableProviderCredential`) scope. No new public endpoint is enabled. An internal
caller owns claim/fingerprint/replay handling and **must reauthorize before replay**.
A future exposed mutation must require an idempotency key and fingerprint the
exact canonical intent (including numeric reference/expected epoch), not redacted
`fmt` output. Do not hash actual key bytes. Without a completion, expected epochs
prevent duplicate changes but a lost successful response retries as conflict,
not replayed success. No exactly-once network or instant secret-recall claim.

## Compatibility, rollback and verification

Additive tables only; existing sessions/agent/runner contracts are unchanged.
Fake sessions stay key-free and real Claude factory selection remains disabled.
No historical credential bindings are fabricated. Down **destroys metadata**;
this is not a safe downgrade once real custody/delivery exists. Reapply creates
empty tables. No privileges on existing tables or dependency versions change.

Synthetic unit/race tests cover role/config/reference/epoch checks, forged verifier
scope, unsafe diagnostics, cooperative timeout/cancellation and serialization.
Disposable PostgreSQL/application-role tests cover lifecycle, actual owner/admin
writes, member removal during verification, late epoch changes, cancellation,
resource ownership across versions/tenants, RLS with no/wrong tenant, immutable
history/state/identity/FK guards, concurrent create, audit/render/claimant rollback,
safe cache replay and migration Down/Up with data. Database cleanup exercises
workspace cascades. No shared development migration or cloud resource is changed.

Eight deliberate mutations were caught by assertions: forged workspace proof,
viewer permission bypass, debug reference disclosure, resource SELECT RLS bypass,
skipped epoch guard, mutable version reference, ignored completion fence and raw
storage diagnostic disclosure. Broader verification results are in the tracker.

## Next

This metadata slice is reviewed/merged. Separately define/verify the current GCP
SDK/IAM, concrete trusted creation/ownership evidence and numeric-version retrieval
under approved synthetic resources. Authenticated delivery/capability acceptance,
allocation fencing and convergent cleanup remain slice (d). Explicit project,
region, replication, IAM and spend decisions precede provisioning/key access.
