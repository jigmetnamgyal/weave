# M6.1b.2c.2 — GCP Secret Manager SDK/API contract review

**Status: docs-only review draft, pending owner/orchestrator review.** This is the
bounded vendor contract review after PR #38 (`cf92599`), not dependency approval,
cloud access, IAM consent, or runtime implementation. It splits the earlier
spec 31 c.2 bundle: an unwired synthetic adapter is a separate, later slice.

## Outcome and limits

Recommend a concrete SDK candidate and record which parts of specs 29–33 are
supported by current public source versus still hypotheses/live gates. No source
code, module/lockfile, migration, cloud client, factory, endpoint, IAM role,
service identity, resource, key access, paid call or real-provider activation is
added here. Do not run authenticated GCP API calls or credential discovery.

## Candidate — not yet selected for implementation

Candidate: **`cloud.google.com/go/secretmanager v1.22.0`, `apiv1`, gRPC client**.
The public Go module proxy's `@latest` metadata returned v1.22.0, published
2026-09-24, at source commit `8a17bee208939e0166936a59675414439c47e341`.
Its module declares Go 1.26.0; the repository toolchain is Go 1.26.8. The module
license is Apache-2.0. deps.dev metadata for this module version listed no
advisory keys at review time; this is not a repository vulnerability scan.

Review performed 2026-10-01. The public v1.22.0 archive was retrieved through the
Go proxy, unpacked under a temporary directory, inspected, then removed. Inspected
files were `go.mod`, `LICENSE`, `apiv1/secret_manager_client.go`,
`apiv1/secretmanagerpb/service.pb.go` and
`apiv1/secretmanagerpb/resources.pb.go`. The proxy's immutable origin commit is
recorded above; no archive checksum was computed. This was public source retrieval,
**not** Go module resolution, `go mod download`, Go module-cache installation,
`go get`, a temporary module/project, or a repository dependency change.

The public module `go.mod` declares 8 direct and 22 indirect requirements:

- Direct: `cloud.google.com/go/iam v1.11.0`, `github.com/googleapis/gax-go/v2 v2.23.0`,
  `google.golang.org/api v0.287.1`, `google.golang.org/genproto` at
  `v0.0.0-20260319201613-d00831a3d3e7`,
  `google.golang.org/genproto/googleapis/api` at
  `v0.0.0-20260630182238-925bb5da69e7`,
  `google.golang.org/genproto/googleapis/rpc` at
  `v0.0.0-20260630182238-925bb5da69e7`, `google.golang.org/grpc v1.83.2`,
  `google.golang.org/protobuf v1.36.11`.
- Indirect: `cloud.google.com/go/auth v0.20.0`,
  `cloud.google.com/go/auth/oauth2adapt v0.2.8`,
  `cloud.google.com/go/compute/metadata v0.9.0`,
  `github.com/cespare/xxhash/v2 v2.3.0`, `github.com/felixge/httpsnoop v1.0.4`,
  `github.com/go-logr/logr v1.4.3`, `github.com/go-logr/stdr v1.2.2`,
  `github.com/google/s2a-go v0.1.9`,
  `github.com/googleapis/enterprise-certificate-proxy v0.3.17`,
  `go.opentelemetry.io/auto/sdk v1.2.1`,
  `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.67.0`,
  `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.67.0`,
  `go.opentelemetry.io/otel v1.44.0`, `go.opentelemetry.io/otel/metric v1.44.0`,
  `go.opentelemetry.io/otel/trace v1.44.0`, `golang.org/x/crypto v0.55.0`,
  `golang.org/x/net v0.58.0`, `golang.org/x/oauth2 v0.36.0`,
  `golang.org/x/sync v0.22.0`, `golang.org/x/sys v0.47.0`,
  `golang.org/x/text v0.41.0`, `golang.org/x/time v0.15.0`.

This is the candidate module's declared requirement set, **not** the complete
resolved build list or license inventory in this repository. No Go module graph
was resolved and no Go command downloaded/installed the SDK or dependencies into
a module cache. Exact MVS upgrades/additions, full transitive license/vulnerability
scan, binary footprint and dependency-policy approval remain a separate explicit
gate. Do not infer consent to add this SDK. The Apache-2.0 and no-advisory result
above apply to the SDK module record only, not its dependency closure.

## Verified public API/SDK contract

The current public Secret Manager v1 discovery document uses the fixed global
base URL `secretmanager.googleapis.com` and exposes both global
`projects/{project}/secrets/{secret}` and regional
`projects/{project}/locations/{location}/secrets/{secret}` resource families.
Spec 30's reference has no location field, so this contract accepts **global only**;
regional names are rejected, not guessed or mapped. The API description accepts
`latest` as an alias for version reads/access, and `AccessSecretVersionResponse`
contains a resource `name` plus a payload. Secret-version IDs begin at 1 and
increase. A request must therefore be constructed from the approved numeric
version, never `latest`, an alias, list/search, a caller-supplied URL or endpoint.

Both `Secret.createTime` and `SecretVersion.createTime` are documented output-only
`google-datetime` values. The SDK maps them to protobuf timestamps with integer
seconds and nanoseconds, matching spec 32/33's exact tuple storage; do not round to
PostgreSQL microseconds. `SecretVersion.state` is one of `ENABLED`, `DISABLED`,
`DESTROYED` or invalid/unspecified. The public schema says only ENABLED may be
accessed; destroyed data is not recoverable. Verify the exact version is ENABLED
before access and fail closed on every other/unknown state. State read and payload
access are separate calls; they do not form an atomic cloud snapshot.

Secret payloads are bytes, with a documented vendor maximum of 64 KiB. The API
schema marks `dataCrc32c` optional and says the service stores/generates a checksum
for later access responses; Google's Go example computes Castagnoli CRC32C over
`payload.data`. In the pinned generated protobuf, `DataCrc32C` is `*int64`, so
presence (including a present value of zero) is distinguishable. The future adapter
must require a nonnil value in `[0, 2^32-1]` and match the Castagnoli checksum of
the exact returned bytes before any consumer sees them. CRC detects transport
corruption; it is not resource ownership or authenticity proof.

The inspected SDK source establishes important defaults:

- `NewClient`'s gRPC default endpoint is `secretmanager.googleapis.com:443`.
  The source also supports configurable endpoint/universe options; the adapter
  must expose no reference-controlled endpoint and must pin the global endpoint.
- `AccessSecretVersion` has a default 60-second GAX timeout and retries gRPC
  `Unavailable`/`ResourceExhausted` with 2-second initial and 60-second maximum
  backoff (multiplier 2). The REST client defaults to retrying HTTP 503/429.
  `GetSecret`/`GetSecretVersion` also default to 60-second call timeouts.
- The public `Client.CallOptions` field can be configured before use, but is not
  safe to mutate concurrently with calls. The gRPC constructor itself sets
  `MaxCallRecvMsgSize(math.MaxInt32)`, not a 16 KiB cap. Caller dial options can
  set another receive limit, but effective precedence over that SDK default needs
  a regression test before relying on it.
- The SDK supports both gRPC and REST clients. Only the gRPC client is a candidate
  here; REST must not be an accidental fallback because its response bound has
  not been established in this review.

The supervisor's existing exact input contract is 1–512 bytes for `APIKey`,
rejecting NUL/CR/LF; Go `len(string)` counts bytes, and the value is forwarded as
an environment string without trimming or Unicode normalization. A future adapter
can check the raw `[]byte` length and forbidden bytes, then convert to a Go string
without changing its bytes. This describes a possible boundary, not an implemented
GCP-to-supervisor handoff. Do not exceed 512 bytes or claim secure zeroization.

## Proposed adapter boundary (still unwired)

Keep three authorities distinct:

1. The existing metadata-only `ProviderReferenceVerifier` remains incapable of
   returning payload bytes. It resolves a current, exact protected approval and
   checks only that global Secret/SecretVersion metadata match the approved scope.
2. A separate, unwired manager-only payload accessor may accept an exact selected
   approval and a bounded callback for synthetic contract tests. Spec 31 explicitly
   says there is no allocation authorization/selection port yet; this slice cannot
   assert current runner/session/binding epoch or authorize backend delivery. Until
   slice (d) supplies that authority, the accessor has no production caller/factory.
   No string/key is returned in an activity result, workflow/outbox/NATS message,
   database row, audit, log, trace, metric label, URL, argv, repository file or
   image/snapshot.
3. The existing protected ledger's restricted reader/writer database capabilities
   remain separate from cloud IAM. No API, general worker, proxy, NATS or sandbox
   identity receives ledger-writer or Secret Manager payload permission.

Metadata verifier and payload accessor should be separate adapter types and use
separate principals. The verifier must lack `secretmanager.versions.access`.
The manager's payload grant must be at the individual Secret resource, not project
scope. Google's `roles/secretmanager.secretAccessor` grants
`secretmanager.versions.access` and is grantable at a Secret; exact metadata-only
permissions/role binding and whether a narrower custom role is needed remain an
IAM review decision. Do not apply IAM here.

Google ADC's documented search order includes `GOOGLE_APPLICATION_CREDENTIALS`,
a local gcloud ADC file, then the attached service account. A bare default client
therefore does not satisfy ADR-019's prohibition on implicit local/operator
credentials. The adapter must receive an explicitly constructed native workload
identity/token source; it must not invoke ambient ADC, inspect credential files,
inherit static service-account JSON, or select a local login. The exact supported
v1.22.0 SDK construction hook and negative proof against ADC fallback were not
validated in this review and are a **blocking acceptance gate**. SDK identity
token acquisition, metadata-server contact/headers, and proxy/environment behavior
were not audited. Production identity must be isolated to the runner manager as
ADR-019 requires, not shared with less-trusted services on a VM.

## SDK telemetry and identity side channels (not verified)

The inspected generated client adds the requested resource name to
`x-goog-request-params` and, when GAX logging/tracing/metrics features are enabled,
adds `resource_name` telemetry context. The source has feature-gated logger and
telemetry hooks, but whether project defaults/environment activate them and what
production exporters retain was not audited. The request path/header necessarily
contains the resource name to address the approved resource; this is not consent to
copy it into Weave logs, traces or metrics. Authentication token acquisition,
metadata-server request details and the gRPC client's behavior with ambient proxy
environment variables were also not verified. Synthetic acceptance must force
these feature toggles on with captured sinks, inject ambient credential/proxy
settings, and prove there is no credential-file/ADC fallback, unexpected proxy
route or resource-name emission from Weave instrumentation. Keep SDK request/body
logging and unsanitized telemetry disabled until that test passes.

## Spec 31 limits: candidate capability is not yet proven

**Retries.** Spec 31 requires no automatic SDK retries. The generated GAX retry
policy is held in public `Client.CallOptions` and appears overridable; a counted
fake-RPC regression must verify that the default GAX retries are actually removed.
`grpc.WithDisableRetry` disables configured gRPC/service-config retries. The pinned
gRPC source also documents transparent retries when no data was written or the
server reports the RPC unprocessed. These are distinct from the SDK/GAX retry
policy and do not constitute an exactly-once guarantee. Acceptance must prove no
configured SDK/GAX/service-config retry; it must separately observe/report
transparent transport behavior. Only if a stronger “no transport replay” rule is
proposed would spec 31 need owner clarification or another transport review.

**16 KiB response cap.** gRPC exposes `MaxCallRecvMsgSize(16*1024)`, but the
SDK's default is `math.MaxInt32`; it is not bounded out of the box. The candidate
control is the **serialized, uncompressed gRPC response message** limit. It excludes
the gRPC frame, HTTP/2 headers and trailers; it is not a total wire-byte cap and
does not itself promise a 16 KiB heap-allocation ceiling. Compression expansion,
protobuf decoding and metadata/object allocation can raise peak memory. Effective
option precedence and allocation behavior were not verified. Synthetic tests must
send serialized messages exactly at and one byte over the limit, with compressed
and uncompressed cases, and measure/document relevant buffering. If spec 31's
pre-full-allocation requirement cannot be substantiated, stop and revise the
transport/contract before adapter composition. Do not describe framing as part of
`MaxCallRecvMsgSize`; keep REST unavailable unless equivalently bounded.

**Exact-byte bound.** The SDK returns raw `[]byte` payloads, so exact-byte CRC and
512-byte validation are implementable after transport receipt. It cannot make the
vendor stop returning its larger 64 KiB maximum; the transport receive cap must
bound that earlier allocation. The future implementation must reject empty,
513-byte, NUL, CR and LF payloads, preserve accepted bytes byte-for-byte through
the callback/supervisor boundary, and never log or format the payload. No live key
is needed for these tests.

**Deadlines/concurrency.** Retain spec 31's five-second accessor/borrow budget
and 8-request manager ceiling as proposed values, not measured production behavior.
The budget covers semaphore wait, explicit identity acquisition, metadata reads,
payload access and a bounded cooperative consumer/hand-off callback. It does **not**
cover running the Claude supervisor; `Run` may last up to eight hours and is a
separate process lifecycle. No supervisor invocation, backend handoff or allocation
authorization is implemented in this contract slice. Configure the context deadline
shorter than SDK defaults; cancellation while waiting must make zero cloud calls.
Configure call options once before sharing the client. These values need synthetic
tests and later capacity measurements; do not add unbounded goroutines.

## Evidence status and live gates

| Finding | Status |
| --- | --- |
| SDK version, Go directive, module license, declared requirement list and generated defaults | Verified from public Go proxy/module source; not installed or added |
| Global and regional v1 resource forms, `latest` alias, output name fields, create-time fields, states, checksum schema, 64 KiB service payload ceiling | Verified from public v1 discovery/API docs and generated protobuf source |
| SDK retry defaults and `math.MaxInt32` gRPC receive default; gRPC configured-retry vs transparent-retry distinction | Verified from pinned public SDK/gRPC source |
| Exact response canonicalization to numeric project number and exact global version path | **Unverified**; API docs give wildcard resource-name formats, not this canonicalization guarantee. Require exact expected-name equality in tests and approved synthetic live acceptance; do not normalize by suffix |
| Timestamp source precision/uniqueness and same-name deletion/recreation continuity | **Unverified**; no public guarantee established in this pass. Deletion/soft-delete/recreation behavior is not inferred. Treat timestamps as observations, not sole identity proof; ledger tombstones remain necessary. Must pass approved synthetic live tests or owner must redesign proof |
| Explicit native identity construction without ADC/environment/static JSON fallback | **Unverified SDK integration detail**; resolve in the synthetic adapter review, then separately approve native identity attachment |
| GAX/service-config retry suppression, transparent gRPC retry observation, 16 KiB message ceiling and pre-full-allocation behavior, effective secret-level IAM, cloud outage/deadline behavior | **Unverified acceptance gates**; no cloud/IAM test performed |
| SDK telemetry/logging of resource names, identity metadata-server acquisition and ambient proxy behavior | **Unverified defaults/behavior**; add negative tests before use |

No live gate here authorizes resource creation, IAM changes, service-role
attachment, project selection, replication/region choice, spend, access to any
customer/operator key, or paid provider invocation. Restore reconciliation and
slice (d) allocation/handoff/cleanup remain required before any production use.

## Exact acceptance for a separate unwired synthetic adapter slice

Only after this review and explicit dependency-policy approval, build a synthetic,
unwired adapter (no production factory or cloud credentials) with the pinned
candidate or a newly approved alternative. Run against an in-process fake gRPC
service / fake approval ledger; do not resolve/download a temporary module graph.
Tests must prove:

- Constructor refuses absent/typed-nil explicit identity, ledger, endpoint or
  configuration; fake ambient credential-file/env inputs are never read; no
  operator ADC fallback. The verifier identity cannot call payload access. With
  SDK tracing/logging/metrics toggles enabled and injected capture sinks, prove
  resource names never enter logs, trace attributes or metric labels. Inject ambient
  credential/proxy environment settings and confirm the intended identity/network
  route without contacting cloud metadata or a real proxy.
- Exact global canonical request path from numeric project, UUID and positive
  decimal version; reject `latest`, aliases, project mismatch, regional path,
  unexpected response name and noncanonical response. Any approval/scope denial
  causes zero metadata/payload RPCs as appropriate.
- Missing/wrong/withdrawn approval, non-enabled version, missing timestamps,
  malformed/nanosecond-boundary timestamps, timestamp chronology and exact resource
  identity mismatch map to fixed safe categories; no raw SDK/transport detail
  escapes. Do not claim current runner/session/binding allocation authorization in
  this slice; that is a later slice (d) gate.
- Checksum is required (including distinguishing absent from present zero), in
  uint32 range, and Castagnoli-correct over the raw bytes. Corruption/CRC failure
  is refused before the callback.
- Byte cases: empty, 1, exactly 512, 513, NUL, CR, LF, invalid UTF-8, and a
  synthetic byte sequence with non-ASCII bytes. Accepted bytes reach the callback
  unchanged; rejected values produce no supervisor start. Owned buffers are
  cleared best-effort only; tests and docs do not claim secure zeroization.
- Serialized, uncompressed protobuf response sizes exactly 16 KiB and 16 KiB + 1
  sent through gRPC; exercise compressed and uncompressed forms, unknown protobuf
  fields and metadata expansion, while measuring frame/header/trailer sizes
  separately. Measure peak buffering; do not infer heap limits from
  `MaxCallRecvMsgSize`. If spec 31's pre-full-allocation requirement cannot be
  substantiated, stop and revise the cap/transport contract before composing the
  adapter.
- GAX/service-config retry policy produces no configured repeat for retryable
  statuses. Separately exercise gRPC transparent-retry cases and report observed
  behavior; do not conflate transport transparency with SDK retry policy or claim
  exactly-once.
- At most eight calls in flight; queue wait, token-source, RPC and consumer share
  one five-second deadline; cancelling while queued causes zero RPCs. Test success,
  denied, unavailable, timeout and consumer cancellation without leaked permits.
- Synthetic secret markers in SDK errors, callback errors, `fmt`, JSON/text
  serialization, logs, tracing and metrics never appear; SDK request/body logging
  is disabled. Raw resource names and evidence remain restricted too.

Live acceptance is another separately approved slice using only synthetic,
disposable GCP resources and explicitly approved principals. It must verify exact
response-name behavior, actual timestamp precision and deletion/recreation
continuity, enabled/disabled/destroyed states, checksum presence/value, effective
secret-level IAM denials for every other service identity, native metadata identity
with ambient credentials absent, quotas/deadlines/cleanup and audit noise. If
GCP does not provide reliable incarnation evidence, or IAM cannot keep metadata
verification payload-blind, stop for an ADR/owner decision; do not weaken the
ledger requirement by assumption. No real provider key or model request belongs
in that live test.

## Public evidence

- [Go proxy latest metadata](https://proxy.golang.org/cloud.google.com/go/secretmanager/@latest), [pinned module metadata](https://proxy.golang.org/cloud.google.com/go/secretmanager/@v/v1.22.0.mod), [pinned source archive](https://proxy.golang.org/cloud.google.com/go/secretmanager/@v/v1.22.0.zip), [immutable source commit](https://github.com/googleapis/google-cloud-go/tree/8a17bee208939e0166936a59675414439c47e341/secretmanager), [pinned API package docs](https://pkg.go.dev/cloud.google.com/go/secretmanager/apiv1@v1.22.0), and [deps.dev module license/advisory record](https://api.deps.dev/v3/systems/GO/packages/cloud.google.com%2Fgo%2Fsecretmanager/versions/v1.22.0).
- [Google Secret Manager v1 discovery document](https://secretmanager.googleapis.com/$discovery/rest?version=v1), [Access a secret version](https://cloud.google.com/secret-manager/docs/access-secret-version), and [Secret Manager access control](https://cloud.google.com/secret-manager/docs/access-control).
- [Google ADC search order](https://cloud.google.com/docs/authentication/application-default-credentials), [Compute Engine service accounts](https://cloud.google.com/compute/docs/access/service-accounts), and [Cloud Run service identity](https://cloud.google.com/run/docs/securing/service-identity).
- [Pinned gRPC retry and receive-size options](https://github.com/grpc/grpc-go/blob/v1.83.2/dialoptions.go); [repository protected ledger](33-protected-secret-approval-ledger.md), [pure approval contract](32-protected-secret-approval-values-and-ledger-policy.md), [GCP boundary contract](31-gcp-secret-verification-and-retrieval-contract.md), [BYOK plan](29-byok-binding-and-delivery.md), [ADR-019](../../docs/adr/0019-gcp-control-plane-and-byok-secret-store.md), [ADR-020](../../docs/adr/0020-trusted-secret-approval-and-creation-evidence.md), and [the current supervisor contract](../../services/runner/agent/claudeprocess/run.go).

## Next

Owner/orchestrator review of this spec and its explicit decisions: candidate
SDK/dependency policy, response-cap semantics/pre-full-allocation feasibility, and
whether vendor creation observations are adequate for the ledger's incarnation
proof. Spec 31's no-automatic-SDK-retries requirement is assessed separately from
gRPC transparent transport retries; request clarification only if a stronger
no-transport-replay requirement is intended. Only after approval, scope the
**separate unwired synthetic adapter**, limited to exact approval scope and bounded
borrow—not allocation authorization or backend handoff. Slice (d) must supply
allocation fencing and receiver delivery before runtime use. Separately approve
later identity attachment or live synthetic-cloud acceptance. No commit, push, PR,
implementation or cloud action is authorized by this draft.
