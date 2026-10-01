# ADR-020: Trusted secret approval and creation evidence

- Status: Proposed; review before schema/adapter implementation
- Date: 2026-10-01
- Scope: M6.1b.2c planning, subordinate to accepted ADR-018/019

## Problem

Spec 30 records immutable references, resource ownership and a verification UUID,
but none proves that a cloud resource belongs to the designated workspace or is
the same resource after deletion/recreation. Labels, a UUID-shaped name, metadata
access and a successful key fetch are not ownership proof. Workspace-cascading
resource rows also cannot enforce lifetime nonreuse after workspace deletion.

Secret-manager version numbers are meaningful within one resource incarnation.
An explicit numeric reference alone is insufficient if a name can be recreated.
This is a design requirement, not a claim about observed GCP recreation behavior.

## Proposed decision

Introduce a **separate protected onboarding approval authority**, not another
owner/admin request field or generic `SystemActor` operation. It records an
immutable approval for an exact environment/project/resource incarnation,
workspace/provider, API-key kind and numeric version incarnation. Registration
and retrieval consult that authority and independently check current cloud
metadata against the recorded creation identity (spec 31).

- A bootstrap identity operates only approved onboarding resources; an approval
  writer records trusted results, not tenant-submitted evidence. Workspaces retain
  provider billing and provider-side revocation responsibilities.
- Ordinary API/worker/proxy/NATS/sandbox identities cannot write approvals or
  access payloads. The metadata verifier may read narrowly scoped approvals/cloud
  metadata; the manager accessor alone receives per-secret payload access.
- The future ledger uses PostgreSQL as metadata truth, separate database grants
  and forced tenant RLS on tenant-bearing approval records. No broad runtime
  writer or generic lookup-by-UUID. A minimal restricted resource tombstone survives
  workspace deletion to prevent reassigning/recreating an approved identifier.
- Cloud creation observations are exact timestamp tuples if vendor validation
  confirms those fields are reliable incarnation evidence. Preserve seconds and
  nanoseconds; do not silently round to PostgreSQL microseconds. Mutable etags and
  labels are not substitutes. If the vendor cannot establish a distinguishable
  incarnation under the approved IAM policy, refuse adoption/activation.
- The opaque verification UUID in spec 30 must resolve to the exact durable
  approval, not an arbitrary nonce. Existing synthetic/unknown evidence cannot be
  promoted to production authority or backfilled from current labels.
- Approval records are immutable. Disablement uses an append-only withdrawal
  decision/current-state projection; approval state is checked on each verification
  and access. It does not replace binding epochs or runtime delivery authorization.

No ledger table, privileged writer, metadata driver, cloud client or deployment is
created by this ADR. Concrete grants, migration/compatibility and recovery require
separate review. The metadata verifier and key accessor are separate capability
objects even if their future implementation shares a package.

## Alternatives

- **Labels/names only:** rejected; a requester could reproduce them without the
  trusted registration decision or creation continuity.
- **Cloud IAM success as tenant proof:** rejected; a manager may access many
  designated resources. Success does not identify the workspace to which one belongs.
- **Key hashes as evidence:** rejected; unnecessary secret-derived durable data,
  no ownership proof, and incompatible with the no-key-in-product-storage boundary.
- **Permanent reliance on deployment manifests:** not selected for production;
  they require equivalent immutability, withdrawal, tombstone and audit machinery,
  and risk bypassing PostgreSQL product-state consistency.

## Consequences and decisions still required

This adds a trusted onboarding control surface and restricted retention burden;
review must approve minimal nonreuse-tombstone retention/deletion policy before
migration. A tombstone is not customer content or a key but is still restricted
metadata. Do not silently retain workspace identifiers or approval actor history
forever: lifecycle, legal/retention policy and access must be explicit.

Public SDK/API evidence must establish metadata identity, creation timestamp
precision, exact version response naming, version state, integrity fields and
workload identity behavior. A writable secret administrator remains privileged:
the design does not defend against a compromised bootstrap/database/cloud control
plane that can rewrite authority or disable safeguards.

This proposal grants no cloud/key access, provisioning, IAM changes or paid-call
consent. Approval of ADR-019's hosting direction does not approve this new ledger's
retention/grants automatically. See spec 31 for the bounded contract and next slice.
