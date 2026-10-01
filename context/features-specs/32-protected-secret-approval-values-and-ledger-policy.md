# M6.1b.2c.1a — Approval values, lookup contract and ledger policy

**Status: pure values/resolver merged in PR #37 (6cc6399). Owner confirmed the
retention and separate database-capability policy; ledger migration/implementation
is merged as cf92599 (PR #38, spec 33), with no deployed authority or
cloud/runtime access.**

Builds on spec 31/proposed ADR-020 and merged spec 30. No vendor SDK/API behavior
is established by these tests.

## Outcome and scope

Implement exact creation observations, read-only scoped approval values and a
consumer-boundary resolver for a future protected authority. Validate closed
schema/family/kind, scope, exact selection, cancellation and safe diagnostics
without database/cloud access. Define the next ledger's concrete retention/grants
for an explicit decision before any DDL.

Included code:

- `domain.SecretCreationTime`: private seconds/nanoseconds, exact tuple comparison;
  post-Unix-epoch through year 9999, canonical nanos, no rounding or zero/missing
  observation. This bounded application format is not a verified GCP type.
- `domain.SecretApprovalScope`: required workspace, Claude provider and canonical
  numeric reference. Shape is not caller authorization or ownership proof.
- `domain.SecretApprovalRecord` and read-only `ProviderSecretApproval`: schema 1,
  `gcp_global`, `api_key`, exact creation observations and nonnil approval ID;
  version observation must not precede resource observation. Getter copies cannot
  retarget the value. Timestamps/evidence mask fmt and refuse JSON serialization.
  Individual field access still requires trusted-code logging discipline.
- `application.SecretApprovalReader`: exact scope or exact ID **plus scope**;
  per-call withdrawal/workspace-deletion checks are the authority's responsibility.
  No ID-only read, payload method, default/fake production factory or writer.
- `SecretApprovalResolver`: explicit nonnil/typed-nil-safe construction; validate
  before I/O, compare exact returned scope/ID, refuse malformed/zero evidence,
  suppress raw authority diagnostics, check cancellation before/after read,
  never cache approvals or switch a selected approval to a current one.

No existing verification service/port, factory or runtime is wired to this code.
A fabricated valid value proves only shape; the future reader must derive authority
from protected records. The resolver cannot prove withdrawal on its own or stop a
reader ignoring its context. Callers must authenticate/authorize first and provide
bounded I/O contexts. No role grants arise from these types.

## Non-scope and compatibility

No migration, sqlc changes, privileged DB/cloud role creation, key bytes/accessor,
SDK/dependency, API route, event/outbox, agent/runner/backend contract or real Claude
activation. Missing ledger means no authority, not an approval fallback. Existing
spec 30 binary behavior and synthetic witnesses remain unchanged; they cannot be
promoted into real approvals. Fake sessions remain key-free.

## Accepted retention decision — owner confirmed after PR37

1. **Minimal nonreuse root retained indefinitely**, including after workspace
   deletion: environment, numeric project, opaque secret UUID, fixed resource
   family, and monotonic reservation state (`reserved`, `consumed`, `retired`).
   No workspace/user/provider-account identifiers, actor identity, key/hash,
   labels, prompts, creation timestamps or approval bodies in this global root.
   It is restricted inventory, not an ordinary tenant table. The opaque UUID may
   remain linkable through external inventory; this is not an anonymization claim.
2. **Tenant assignment, approval and withdrawal records cascade on workspace
   deletion**, like existing binding history. They contain workspace/provider,
   exact creation observations, version/approval IDs, approved intent/principal
   attribution and decision times. They do not survive in the nonreuse root.
   Backups follow separately defined environment backup/retention policies;
   deleting active rows is not immediate deletion from backups. Operational
   audit retention is not silently redefined by this proposal.
3. Root reservation commits before external creation. A unique project/secret UUID
   can never be reserved again (even for another environment/workspace). Assignment
   may consume a reserved root exactly once, atomically with the tenant assignment;
   consumed state remains after that child cascades. Failed/ambiguous onboarding
   retires the root and cannot release/recreate/reassign it. A never-resolved
   reservation remains unavailable to a different intent.
4. Existing assigned resources can receive new exact version approvals only for
   the original live assignment/incarnation. Workspace deletion, withdrawal or
   missing records refuses future lookup. No reconstruction from labels or cloud
   reads; no resurrection after deletion/restore.

The owner explicitly confirmed this policy after reviewing spec 32, separately
from merging spec 31. Implementation, backup/operational handling and deployment
review remain required; no blanket data-retention change for other records.

## Persistence/grants direction accepted — implementation/compatibility review required

Future additive tables (names provisional):

- `provider_secret_reservations`: global nonreuse root above; immutable identity,
  unique project/UUID and one-way guarded reservation state.
- `provider_secret_assignments`: tenant-scoped immutable assignment and exact
  resource creation observation, matching one consumed root. Only one assignment
  may ever consume a root; losing its workspace child does not reset root state.
- `provider_secret_approvals`: tenant-scoped immutable per-version evidence, exact
  version creation observation, schema/kind/family/approval ID and intent/principal
  attribution, with composite owner/resource/workspace FK and version uniqueness.
- `provider_secret_withdrawals`: append-only scoped decisions; reads exclude any
  approval with an effective withdrawal. A withdrawal cannot be removed to revive
  evidence. Resource-wide retirement must affect all its approvals; concrete
  projection/serialization is defined before implementation.

Proposed **NOLOGIN** database capability roles, distinct from cloud IAM:

| Principal | Proposed access |
| --- | --- |
| `weave_app`, API/worker/proxy/NATS/sandbox DB callers | No new ledger privileges or membership in capability roles |
| `weave_secret_approval_reader` | SELECT tenant assignment/approval/withdrawal data under forced RLS and exact scoped predicates; no global inventory or writes |
| `weave_secret_approval_writer` | Scoped SELECT/INSERT for approved onboarding/withdrawal, root reservation and guarded state-column update only; no tenant UPDATE/DELETE/TRUNCATE or role administration |
| Migration owner | Creates schema/guards/roles; not a runtime identity |

No login/user/service account is created or granted role membership by this unit
or assumed in a future migration. Deployment must separately approve attachment
to narrowly scoped trusted onboarding/verifier identities. `SystemActor` or a
workspace-owner role does not become an approval writer. Required pools and
principal/config dependencies fail startup if missing, including typed-nil values.

Tenant tables get enabled/forced RLS, exact workspace predicates, composite FKs,
append-only mutation/TRUNCATE guards and workspace cascade exceptions. Root policy
is project/environment-scoped for the trusted writer and denies general runtime
inventory reads; no tenant identity is added to the root merely to fit tenant RLS.
Tenant GUC scope is defense in depth, not authentication: the privileged process
must derive scope from approved intent/native identity, never inbound claims.

External cloud operations remain outside short ledger transactions. Reserve,
consume/approve and withdraw are separately fenced/idempotent decisions with safe
audit/intent IDs; no raw request/provider body. Approval publication needs verified
creation observations, not a placeholder successful reservation. Namespace/intent
ambiguity retires/refuses rather than republishing another resource under an old ID.

Do not add a mandatory FK from existing `verification_id` that would break previous
spec 30 writers or fabricate approvals. Unknown/synthetic evidence remains allowed
as old metadata but refused by real authority lookup. Production composition still
fails closed until compatible readers/writers, cloud verification and slice (d)
allocation/handoff/cleanup gates are all present.

Rollback must not erase nonreuse authority: leave additive tables on binary
rollback; Down may remove an empty ledger only. Dropping populated reservations
requires a separate destructive recovery decision, not a routine downgrade. Restore
must quiesce onboarding/access, preserve name reservations/withdrawals and refuse
uncertain reconstructed authority; older backups cannot silently revive approvals
or free names. Concrete restore reconciliation and backup retention precede live use.

## Acceptance and evidence

Current synthetic tests: exact nanos/seconds round-trip, invalid/zero/bounds,
chronology, schema/family/kind/provider/scope, getter copies, fmt/JSON refusal;
resolver wrong workspace/environment/project/resource/version/ID, missing/withdrawn
projection, no cached fallback, unknown/poisoned errors, cancellation/late success,
nil/typed-nil authority and zero calls for invalid intent. These are **not real
ledger permission/RLS or cloud-ownership tests**. Mutation/fuzz results recorded
in the tracker.

Next c.1b under the confirmed retention/grant decision: additive ledger/schema/
sqlc/protected store, isolated actual-role positive/negative tests, concurrent
reservation/assignment/withdrawal, permanent nonreuse after tenant cascade,
rollback/restore gates, explicit scoped readers and safe error/audit/idempotency.
No SDK/key bytes in c.1b. Current vendor/identity/creation timestamp behavior and
bounded secret access remain later separately reviewed units.


Protected ledger implementation and concrete retry/rollback/security evidence are
tracked in [spec 33](33-protected-secret-approval-ledger.md). Deployment/identity
attachment, restore reconciliation and cloud acceptance remain gated.
