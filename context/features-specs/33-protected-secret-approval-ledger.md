# M6.1b.2c.1b — Protected approval ledger

**Status: implemented on `m6.1b2c1b-approval-ledger`, awaiting review. Unwired;
no deployed authority, GCP access or real Claude activation.**

## Outcome, scope and security

Implement spec 32's accepted retention/capability policy with additive migration
00018, sqlc and explicit PostgreSQL reader/writer adapters. No route, service
factory, login attachment, cloud SDK/IAM/resource, key bytes or runtime composition.
Existing spec 30 binaries and synthetic witnesses remain compatible; no backfill,
mandatory verification FK or fabricated promotion into approval authority.

Five tables:

- `provider_secret_reservations`: permanently unique project/secret UUID, environment,
  fixed `gcp_global` family and one-way state only. No tenant, actor, timestamps,
  key/hash or approval body. Reader/app have no inventory grant.
- `provider_secret_intents`: immutable tenant/provider/reference, initial numeric
  version, stable intent ID, trusted principal attribution and decision time.
  This separate pending-intent record prevents unresolved onboarding reassignment.
- `provider_secret_assignments`: one immutable resource observation per intent,
  seconds/nanoseconds as integers, never `timestamptz`-rounded evidence.
- `provider_secret_approvals`: immutable exact numeric-version observations, schema
  1/API-key kind/approval ID and attribution. Composite tenant/intent/assignment FKs
  and one approval per intent/version prevent new IDs reviving withdrawn versions.
- `provider_secret_withdrawals`: append-only approval-specific or whole-resource
  decisions with stable decision/principal IDs. Whole-resource withdrawal retires
  the name atomically, including abandoned pending onboarding without evidence.

All tenant rows cascade on workspace deletion. The root remains reserved, consumed
or retired forever, with no retained tenant attribution. This is restricted,
potentially linkable inventory, not anonymization or backup erasure.

Dedicated `NOLOGIN NOINHERIT NOSUPERUSER NOBYPASSRLS NOCREATEROLE NOCREATEDB
NOREPLICATION` reader/writer roles receive narrowly scoped grants; no membership
or login is created/attached. Unsafe existing role attributes or ordinary app
membership cause migration refusal rather than silently normalizing deployment
identity policy. Forced RLS requires workspace and configured project/environment
on tenant evidence, and project/environment on root writes. No SECURITY DEFINER
or additional bypass capability is introduced. GUCs are **not authentication**;
the privileged process must derive scope from approved intent/native identity.

Constructors validate nonnil pools, bounded canonical config, exact actual role,
non-bypass attributes and required privileges; writer principal attribution is an
explicit nonnil trusted UUID, never a request-body actor. Pools/config do not prove
native identity authentication. Deployment attachment/authenticated onboarding
remains separately gated. Transactions preserve `TenantFrom(ctx)` rather than
silently adopting a requested workspace.

## Decisions, fencing and failure behavior

`ReserveIntent` commits the new root and exact tenant intent together **before**
external creation, leaving state `reserved`. A first intent can attach only to a
root physically INSERTed in that same transaction (`xmin = pg_current_xact_id()::xid`),
under its row lock; this is a live current-transaction comparison, not durable
numeric txid authority. State changes cannot refresh a root back to reserved.
An orphaned reserved root after tenant cascade therefore cannot accept another
intent. Deferred guards forbid committing a reserved root without an intent.
Names are never released or recreated. Ambiguous onboarding must retire/refuse,
not change an intent to adopt another resource.

`AssignResource` locks the root and transitions reserved → consumed atomically
with the immutable tenant creation observation. `PublishApproval` locks the same
root, validates exact scope/resource observation, checks chronology in Go and
PostgreSQL, and publishes only under a consumed resource. `Withdraw` shares that
lock; whole-resource decisions transition reserved/consumed → retired. Deferred
guards forbid consumed roots without assignment or retirement without a durable
withdrawal when tenant intent exists. Reader projection excludes both version and
resource withdrawals. No current-binding/cache/ID-only fallback exists.

Exact retries retain caller IDs/parameters and trusted principal. Changed intent,
assignment observation, approval scope/ID/content or withdrawal target refuses.
An identical historical decision retry may acknowledge that immutable decision
without restoring usability. Concurrent first reservation attempts can return a
conflict; retry only the unchanged stable intent, never invent another name or
reinterpret an ambiguous external result. There is no exactly-once cloud claim.

Each adapter transaction has a five-second local budget (caller deadlines may be
shorter), no external I/O/callback, and rollback on failure. Invalid/foreign/absent
or conflicting evidence maps to reference rejection; other DB diagnostics map to
safe authority-unavailable errors. Private SQL details and cancellation causes are
not returned. A cancelled/unknown commit acknowledgement requires an exact retry,
not an assumption that nothing committed. Protected immutable decision rows are
the slice's audit: actor/intent/approval/decision IDs and decision times commit
with state. No generic app audit/outbox permission or exposed API is added.

## Compatibility, rollback and restore gate

Binary rollback leaves the additive ledger. Down refuses **any** nonreuse row,
including names whose workspace has disappeared; an empty ledger may be removed.
It revokes helper/schema grants and drops tables/functions, leaving cluster-wide
capability roles with no login/membership attachment. Destructive recovery is a
separate approval, never a routine Down or cleanup script.

In-place root/evidence mutation and TRUNCATE guards block ordinary attempted
resurrection/erasure, including owner-bypass SQL tests. They do **not** defend
against a database/control-plane administrator dropping safeguards or restoring
an older database image. No stale-backup reconciliation/high-water mechanism is
implemented or verified here. Before any live authority composition, quiesce
onboarding/access, reconcile every nonreuse identifier/withdrawal against current
trusted inventory/history, refuse uncertain authority, and approve/test the
backup/restore runbook and native identity attachments. Until then production
composition remains disabled. Existing backup/audit retention is unchanged.

## Acceptance and verification

Seven disposable actual-role integration cases cover lifecycle/exact retries,
ID+scope reads, nanosecond evidence, native grants/RLS and constructor refusal,
immutability/withdrawal/cascade, atomic failure/publication-vs-withdrawal,
empty/populated Down/Up and privilege lifecycle, pending-name nonreuse after
cascade, concurrent cross-tenant reservation and cancellation. Raw role SQL
checks complement adapter predicates; synthetic observations are **not cloud
ownership or vendor timestamp evidence**. Existing provider-binding/input tests
and full serialized integrations also pass with migration 00018.

Commands (integration URLs must target isolated, disposable test databases):

```sh
make sqlc
make lint-go
go test -race -p=1 ./internal/... ./services/...
go test -race ./internal/adapters/postgres -run SecretLedger -count=3
go build ./services/...
```

Six deliberate schema mutations caught: pending-root reclaim, tenant visibility,
root deletion, retirement publication fencing, one-nanosecond chronology inversion,
and populated Down. Chronology initially survived because Go validation masked
the DB path; a raw actual-writer regression now independently exercises the
constraint and catches the mutation. All mutations restored before passing runs.
No timeout/assertion weakening, dependency or live paid/provider call.

Next: all-surface PR/threat review and explicit merge approval. Only then a separate
pinned vendor/SDK/IAM contract review and authenticated onboarding/native identity,
restore reconciliation and allocation/handoff/cleanup slices.
