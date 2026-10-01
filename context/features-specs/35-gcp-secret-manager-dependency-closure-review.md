# M6.1b.2c.3 — GCP Secret Manager dependency-closure review

**Status: owner-approved app-focused and SDK-only audits completed; documentation-only review.** Baseline: main
`66279f174f3ce313ca738cc3e7f06b91ee7bc11e`. Candidate:
`cloud.google.com/go/secretmanager v1.22.0`. This documentation does not authorize
SDK adoption, adapter work, dependency changes or runtime use.

## Method and reproducibility

The baseline `go.mod`/`go.sum` were copied to owned temporary baseline/candidate
modules. Go 1.26.8 was invoked by its existing cached binary with `GOENV=off`,
`GOTOOLCHAIN=local`, explicit temporary `HOME`, `GOMODCACHE`, `GOCACHE`, `GOPATH`
and `TMPDIR`, `GOWORK=off`, and public `GOPROXY`/`GOSUMDB`. No tidy, repository
module edit, package execution, build or test was run.

```sh
GO=/Users/mac/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.8.darwin-arm64/bin/go
AUDIT=$(mktemp -d /tmp/weave-gcp-dep-audit.XXXXXX)
mkdir -p "$AUDIT/baseline" "$AUDIT/candidate" "$AUDIT/home" \
  "$AUDIT/gomodcache" "$AUDIT/gocache" "$AUDIT/gopath" "$AUDIT/tmp"
export HOME="$AUDIT/home" GOMODCACHE="$AUDIT/gomodcache" \
  GOCACHE="$AUDIT/gocache" GOPATH="$AUDIT/gopath" TMPDIR="$AUDIT/tmp" \
  GOENV=off GOTOOLCHAIN=local GOWORK=off GOPROXY=https://proxy.golang.org \
  GOSUMDB=sum.golang.org GOOS=linux GOARCH=amd64 CGO_ENABLED=0
git show 66279f174f3ce313ca738cc3e7f06b91ee7bc11e:go.mod > "$AUDIT/baseline/go.mod"
git show 66279f174f3ce313ca738cc3e7f06b91ee7bc11e:go.sum > "$AUDIT/baseline/go.sum"
cp "$AUDIT/baseline/go.mod" "$AUDIT/candidate/go.mod"
cp "$AUDIT/baseline/go.sum" "$AUDIT/candidate/go.sum"
(cd "$AUDIT/baseline" && "$GO" list -m -json all > "$AUDIT/baseline.json")
(cd "$AUDIT/candidate" && "$GO" get cloud.google.com/go/secretmanager@v1.22.0)
(cd "$AUDIT/candidate" && "$GO" list -m -json all > "$AUDIT/candidate.json")
# The earlier inventory downloaded only the 170 module-only selections; do not
# expand source downloads to the separate 291-module package-loaded graph.
```

An initial manually edited candidate copy was rejected because `go.mod` needed
updates; the result was resolved using `go get` only in the disposable copy, not
`go mod tidy`. Repository dependency files remain unchanged:
`go.mod` SHA-256 `149be3e5a41a0e3645de7b1a4f08e3a116bb904f365819d8d94a07f3f02d36a2`,
`go.sum` SHA-256 `e22b3dc00ecfa8e62e4631669c5ef8c9d1a6d7772b8e67e5de7c8169c5ddbeed`.

SDK source origin commit: `8a17bee208939e0166936a59675414439c47e341`; module
sum `h1:c9nPLiK4IZeT/zDyLjvNaBw1BHNkp0Ysybj1FfFIAPQ=`; go.mod sum
`h1:aDN9cW5x6Y8QVj32snakZv96vYyW7Nf1P+eqZGH8408=`. Temporary candidate
`go.sum` SHA-256:
`360be7fd287f1381699aafa23420613c69eeaf84c5e9c4b235e2f8fae4a0c473`. Sanitized
170-row selected-module sums and legal-file hash indexes remain LOCAL-ONLY,
ignored and unpublished at `tmp/gcp-secret-manager-dependency-closure-audit-2026-10-01/`;
GitHub reviewers cannot access them. Their SHA-256 values are
`78eb0d9d0f3731c7a5a2d579591b5978c7e37381849f389048227767adb494a7` and
`15924650f5bd048c80d303c7d2ce8413b5e0ca2f8a0616ef35bb862a7bc5e3be`, respectively.

## Module-graph result (not package closure)

MVS selected **164 baseline / 170 candidate modules: 6 added, 5 upgraded, 0
removed or downgraded**.

- **Added:** `cloud.google.com/go/secretmanager v1.22.0`; `cloud.google.com/go/iam
  v1.11.0`; `cloud.google.com/go/auth/oauth2adapt v0.2.8`;
  `google.golang.org/api v0.287.1`;
  `google.golang.org/genproto v0.0.0-20260319201613-d00831a3d3e7`;
  `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc
  v0.67.0`.
- **Upgraded:** `cloud.google.com/go/auth v0.18.2 → v0.20.0`;
  `github.com/googleapis/enterprise-certificate-proxy v0.3.11 → v0.3.17`;
  `github.com/googleapis/gax-go/v2 v2.17.0 → v2.23.0`;
  `golang.org/x/time v0.3.0 → v0.15.0`;
  `google.golang.org/genproto/googleapis/api
  v0.0.0-20260526163538-3dc84a4a5aaa →
  v0.0.0-20260630182238-925bb5da69e7`.

The SDK's module file declares Go 1.26.0, 8 direct and 22 indirect requirements.
The graph adds/upgrades Google API/auth/gRPC, generated API/protobuf, GAX and
OpenTelemetry layers. This is module-selection evidence only—not the packages
imported by `apiv1`, a binary-size estimate, or runtime safety evidence.

## License evidence (heuristic, incomplete)

The selected-source filename inventory covered 170 module trees: 169 had a root
license-named file; **one was missing root evidence**:
`github.com/nexus-rpc/nexus-proto-annotations@v0.1.0` (baseline-existing, not a
candidate addition). Nine modules had `NOTICE`-named files; none of the 11 added or
upgraded versions had one in the inventory. No SPDX ID appeared in legal-named
files. The attempted Go-source-header pass failed with an inventory-script
`KeyError('Path')` before reading headers; source-header SPDX coverage is unknown.

The initial text matcher combined overlapping family matches, so an exact
unmatched-root count is **not asserted**. Its pattern set omitted BSD and ISC, and
its GPL hit in unchanged
`github.com/go-sql-driver/mysql@v1.10.0` was a false positive: the root license is
MPL-2.0 and the GPL phrase discusses a secondary-license option. Broader heuristic
family counts were not accepted as legal conclusions. These counts do not clear
license obligations.

Directly read root license text for the changed selections:

| Source heading/text | Added/upgraded versions |
| --- | --- |
| Apache License 2.0 | `cloud.google.com/go/auth@v0.20.0`; `cloud.google.com/go/auth/oauth2adapt@v0.2.8`; `cloud.google.com/go/iam@v1.11.0`; `cloud.google.com/go/secretmanager@v1.22.0`; `github.com/googleapis/enterprise-certificate-proxy@v0.3.17`; `go.opentelemetry.io/contrib/.../otelgrpc@v0.67.0`; both selected `google.golang.org/genproto` modules |
| BSD 3-clause terms | `github.com/googleapis/gax-go/v2@v2.23.0`; `google.golang.org/api@v0.287.1`; `golang.org/x/time@v0.15.0` |

Source-text observations are not SPDX declarations or legal clearance. Preserve
applicable Apache notices/patent terms and BSD attribution, disclaimers and
non-endorsement terms. Unchanged baseline multi-license files (including
`klauspost/compress` component licenses and YAML's file-scoped MIT/Apache terms)
need package-level attribution; the package closure was not mapped.

## Incomplete gates, resource overshoot and cleanup

The first 1-GiB pass did not complete package/import or vulnerability scanning.
The owner-approved follow-up did complete `go list -deps` package import inventories
for the tracked app at baseline/candidate module selections and for a separate
owned SDK/proposed-auth probe. The pre-existing `govulncheck` v1.8.0 binary was
available (not installed in this slice). Current-app package and symbol scans
completed; the SDK package-loaded advisory scan did not start before the 30-minute
budget expired. The earlier deps.dev result in spec 34 covers only the SDK module. The initial
app-focused pass did not reach the package-loaded SDK advisory or package-license/
NOTICE/SPDX scope; those were completed only in the separate SDK-only pass below.
No production SDK called-safety claim is made.

The 1-GiB aggregate resource check sampled every 20 module downloads, not before
allocation: at 160 modules usage was 1,046,352 KiB; at 170 it was 1,373,636 KiB
(325,060 KiB over budget). After inventories, the owned workspace was
1,373,772 KiB. No module source paths were pruned. The 170 owned zip archives
were 243,630,373 bytes. This cap was not pre-allocation-enforced.

Cleanup was initially blocked by read-only module files. The pointer and exact
owned root were validated; the root was not a symlink and `find -P` found zero
symlink entries. `chmod -R -P u+w` was applied only to that root; the removal retry
succeeded. The root and pointer were verified absent. No normal Go cache was
cleaned. Compact metadata remains LOCAL-ONLY, ignored and unpublished at
`tmp/gcp-secret-manager-dependency-closure-audit-2026-10-01/` (170 selected-module
sums and legal-filename hashes; 112 KiB); it is not reviewer-accessible evidence.

One initial `govulncheck -version` availability check in the previous slice ran
under default `HOME`; whether it touched that DB cache is unknown. In this
follow-up, Go and scanner version checks used fresh isolated `HOME`, XDG and Go
caches. No normal cache was inspected or cleaned.

## Owner-approved 3-GiB follow-up results (2026-10-01)

All Go/scanner activity used the pinned Go 1.26.8 binary, static linux/amd64,
CGO=0, public Go proxy/sumdb and vuln DB, with isolated cache/home paths and a
monitored hard-stop wrapper. No source code, binary, init function, test or
`go generate` ran. No SDK source was added to the repository.

**Tracked app import matrix.** `go list -deps -json` against actual repository
sources and isolated baseline/candidate modfiles completed: baseline and candidate
each had 644 package records, 52 root packages and 55 non-stdlib modules; package
path sets were identical and neither imported `secretmanager/apiv1`. The only
selected-version changes actually present in the candidate app import closure are
`golang.org/x/time v0.3.0 → v0.15.0` and
`google.golang.org/genproto/googleapis/api
v0.0.0-20260526163538-3dc84a4a5aaa →
v0.0.0-20260630182238-925bb5da69e7`. There are no newly imported module paths in
the current app.

**Separate SDK/proposed-auth probe.** A disposable module copied the candidate
module file, then only that probe's `go.mod`/`go.sum` were updated to load
`apiv1`, `option.WithAuthCredentials`, and separately labelled never-executed
synthetic injected/default-auth NewClient+AccessSecretVersion roots. The client type
is `apiv1.Client`. Static `go list -deps` completed with 471 package records and
31 external module versions. This is a static import inventory; the synthetic
functions were never executed and do not establish production called reachability
or select a production auth path.

The candidate root's module-only MVS remains 170 (164 baseline, +6/5 upgrades).
Package-loading the SDK probe expands its selected graph to **291 modules**. Against
the candidate module-only graph this is +121 module requirements, 2 version
upgrades, 0 removals; against baseline it is **127 additions, 7 upgrades, 0
removals**. The two extra changed selections relative to module-only are
`github.com/davecgh/go-spew v1.1.1 →
v1.1.2-0.20180830191138-d8f796af33cc` and
`github.com/pmezard/go-difflib v1.0.0 →
v1.0.1-0.20181226105442-5d4384ee4fb2`; neither appears in the SDK import matrix.
None of the 121 further MVS-only additions is an imported package in the 471-row
probe matrix. They include generated/Google service graph requirements (including
`google.golang.org/genproto/googleapis/bytestream`) and are **selected graph
constraints, not runtime imports**. The complete added/version-changed list and
both probe/module-only `go.mod`/`go.sum` pairs are preserved in the ignored follow-up
evidence bundle linked below. The 170-module graph must not be presented as the
SDK package-loaded selection.

The SDK probe's imported module/version delta against the baseline app package set
is 16 entries: 14 newly imported module paths—`cloud.google.com/go/auth v0.20.0`,
`cloud.google.com/go/auth/oauth2adapt v0.2.8`,
`cloud.google.com/go/compute/metadata v0.9.0`, `cloud.google.com/go/iam v1.11.0`,
`cloud.google.com/go/secretmanager v1.22.0`, `github.com/felixge/httpsnoop v1.1.0`,
`github.com/google/s2a-go v0.1.9`,
`github.com/googleapis/enterprise-certificate-proxy v0.3.17`,
`github.com/googleapis/gax-go/v2 v2.23.0`,
`go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc v0.67.0`,
`go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp v0.71.0`,
`golang.org/x/oauth2 v0.36.0`, `google.golang.org/api v0.287.1`, and
`google.golang.org/genproto v0.0.0-20260319201613-d00831a3d3e7`—plus the two app
module upgrades (`x/time`, `genproto/googleapis/api`) above.

**Vulnerability evidence.** The pre-existing `govulncheck v1.8.0` binary reported
DB `https://vuln.go.dev`, updated `2026-09-28 16:43:40 UTC`; scans ran
`2026-10-01 15:27–15:30 UTC`. Completed module-only scans of baseline 164 and
candidate module-only 170 both returned status 3 with the same three selected-
module findings in `golang.org/x/crypto v0.55.0`: GO-2026-6355 and GO-2026-6354
(fixed in v0.56.0), and GO-2026-5932 (`openpgp` unmaintained/unsafe, no fix).
Status 3 is findings, not scanner failure. This version was baseline-existing;
this audit did not upgrade it.

Current-app package scans completed for baseline and candidate with zero vulnerable
imported packages (status 0), while each also reported those three module findings.
The candidate current-app symbol scan completed with zero reachable/called findings
(status 0), also noting the three module findings as not called. These scans cover
the current app, **not** the unwired SDK probe. The 291-module SDK package-loaded
module scan and SDK package/symbol scans were not started before the supervised
30-minute budget expired; their advisory and called status was then unknown.
That statement refers to the first app-focused follow-up. A later, separately
approved SDK-only pass completed scoped apiv1 module/package and synthetic symbol
scans below. Earlier `-format=json -show=version` attempts and incorrect targets
failed before analysis and remain invocation errors, not findings or passes.

**Package-license evidence.** Comparing the 471-row SDK import inventory to the
baseline app identifies the 16 module/version entries listed above. All 16 have
root legal-named file/hash evidence in the retained candidate inventory; this is
not package-level license clearance. Directly read headings from the earlier
candidate inventory cover 11: the eight Apache-2.0 sources in the prior table and
BSD 3-clause `gax-go`, `google.golang.org/api` and `x/time`. The other five
(`compute/metadata`, `httpsnoop`, `google/s2a`, `otelhttp`, `x/oauth2`) have only
root filename/hash evidence here; their headings were not directly classified in
this pass. `x/time` (BSD 3-clause) and `genproto/googleapis/api` (Apache-2.0) are
the two changed modules in the current app closure. No imported Go-source SPDX
header pass or package-specific NOTICE-scope attribution was completed. The prior
heuristic/unmatched-count limitation remains; no license clearance is claimed.

**Budget and cleanup.** Maximum observed owned-workspace usage was 838,032 KiB,
below the 3-GiB cap; this is sampled observation, not a guaranteed peak. Monitoring
was not continuous or pre-allocation enforcement; the 512-MiB reserved headroom
and 3-second watchdog are operational safeguards, not OS resource quotas. No
all-291 source download was attempted and no source paths were pruned. After
preserving the module-only and package-loaded graphs/modfiles/sums,
import inventories, advisory summary and root legal-file index, the exact owned
root/pointer were validated, `find -P` found zero symlinks, and owner-write
permissions were changed only under that root. Removal succeeded with zero retry
diagnostics; root and pointer absence were verified. The prior 112-KiB evidence
remains unchanged. This pass exhausted its 30-minute scan budget; no further scans
are implied.

Compact follow-up evidence (module graphs, modfile/sum pairs, import matrices,
scan metadata and root legal-file hashes) remains LOCAL-ONLY, ignored and
unpublished at `tmp/gcp-secret-manager-dependency-closure-followup-2026-10-01/`;
GitHub reviewers cannot access it.

## Separate owner-approved SDK-only completion (2026-10-01)

The second focused pass reused the preserved 291-row package-loaded MVS, modfile and
sumfile. SHA-256 of both files matched the prior evidence copies; `go list -deps
-json -mod=readonly` completed for the four SDK/proposed-auth/synthetic roots with
471 package records, 31 external modules, no errors, and no imported-version
mismatch against the preserved 291-module MVS. The SDK import-version delta is 11
paths versus baseline MVS164 and 16 entries versus the baseline app's imported
packages. Graph deltas remain baseline164 → SDK-loaded291: 127 added, 7 upgraded,
0 removed; module-only170 → SDK-loaded291: 121 added, 2 upgraded, 0 removed.

The prior compact bundles did **not** contain the original scanned `.go` roots.
The later evidence-copy step flattened source basenames; the injected and default
synthetic roots both used `probe.go`. Consequently, the original scanned
`syntheticroot/probe.go` (injected-auth root) is unavailable in the preserved
bundle. Do not treat any snippet below as retrospective proof of the exact scanned
source: these are clearly labelled reconstructed minimal equivalents for
independent reproduction only. All probe functions were never executed; their
import matrix matched the preserved 471-package manifest and selected versions.
This source-reconstruction limitation does not establish a production auth choice.

```go
// Reconstructed equivalent: sdkapiv1/sdk.go (type-only root)
package sdkapiv1
import "cloud.google.com/go/secretmanager/apiv1"
func ClientType(client *apiv1.Client) *apiv1.Client { return client }

// Reconstructed equivalent: proposedauth/proposedauth.go (option/type root)
package proposedauth
import (
    "cloud.google.com/go/auth"
    "google.golang.org/api/option"
)
func CredentialsOption(credentials *auth.Credentials) option.ClientOption {
    return option.WithAuthCredentials(credentials)
}

// Reconstructed equivalent: syntheticroot/probe.go (injected auth; never run)
package syntheticroot
import (
    "context"
    "cloud.google.com/go/auth"
    apiv1 "cloud.google.com/go/secretmanager/apiv1"
    "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
    "google.golang.org/api/option"
)
func InjectedAccess(ctx context.Context, credentials *auth.Credentials, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
    client, err := apiv1.NewClient(ctx, option.WithAuthCredentials(credentials))
    if err != nil { return nil, err }
    defer client.Close()
    return client.AccessSecretVersion(ctx, req)
}

// Reconstructed equivalent: syntheticdefault/probe.go (default auth; never run)
package syntheticdefault
import (
    "context"
    apiv1 "cloud.google.com/go/secretmanager/apiv1"
    "cloud.google.com/go/secretmanager/apiv1/secretmanagerpb"
)
func DefaultAccess(ctx context.Context, req *secretmanagerpb.AccessSecretVersionRequest) (*secretmanagerpb.AccessSecretVersionResponse, error) {
    client, err := apiv1.NewClient(ctx)
    if err != nil { return nil, err }
    defer client.Close()
    return client.AccessSecretVersion(ctx, req)
}
```

For independent static reproduction, use the candidate module-only `go.mod` and
`go.sum` created in the first command block, but **do not expect that 170-module
file to readonly-load the SDK**. In a separate disposable probe, copy that pair,
place the four reconstructed packages above into their corresponding directories,
then perform the pinned `go get` in that probe **before** readonly package loading;
this temp-only module update is what enables the package-loaded 291 selection.
Run from the probe directory, in the same isolated environment from the first block:

```sh
GO=/Users/mac/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.26.8.darwin-arm64/bin/go
export PATH="$(dirname "$GO"):$PATH" # govulncheck launches go through PATH
VULNCHECK=/Users/mac/go/bin/govulncheck
"$GO" version       # require go1.26.8; GOTOOLCHAIN=local prevents auto-download
"$VULNCHECK" -version # require govulncheck v1.8.0
mkdir -p "$AUDIT/sdk-probe"
cp "$AUDIT/candidate/go.mod" "$AUDIT/sdk-probe/go.mod"
cp "$AUDIT/candidate/go.sum" "$AUDIT/sdk-probe/go.sum"
# Create sdkapiv1/, proposedauth/, syntheticroot/, syntheticdefault/ from snippets above.
(cd "$AUDIT/sdk-probe" && "$GO" get cloud.google.com/go/secretmanager/apiv1@v1.22.0)
cd "$AUDIT/sdk-probe"
"$GO" list -deps -json -mod=readonly ./sdkapiv1 ./proposedauth ./syntheticroot ./syntheticdefault > packages.json
"$GO" list -m -json all > modules.json # compare selected modules with baseline
"$VULNCHECK" -db https://vuln.go.dev -format=json -mode=source -scan=module ./sdkapiv1
"$VULNCHECK" -db https://vuln.go.dev -format=json -mode=source -scan=package ./sdkapiv1 ./proposedauth ./syntheticroot ./syntheticdefault
"$VULNCHECK" -db https://vuln.go.dev -format=json -mode=source -scan=symbol ./syntheticroot ./syntheticdefault
```

The Go path above is the pinned binary used for this audit; reviewers on another
host must set `GO` to an existing Go 1.26.8 binary (never rely on toolchain
auto-download). The exported cache/home/proxy variables from the first block must
remain set, with `GOENV=off`, `GOTOOLCHAIN=local`, and `GOWORK=off`. These are
reproduction instructions only, not fresh scans in this PR. Compare the resulting
291-module selection to baseline commit `66279f174f3ce313ca738cc3e7f06b91ee7bc11e`;
the 31 external-module SBOM is the imported-package scope, not the whole graph.

**SDK-scoped advisory results.** `govulncheck` v1.8.0 used Go 1.26.8 and
`https://vuln.go.dev` (DB last modified `2026-09-28T16:43:40Z`). The completed
module scan targeted `sdkapiv1` at `2026-10-01T16:30:58Z`; package scan targeted
all four roots at `16:32:25Z`; symbol scan targeted only the synthetic injected /
default-auth roots at `16:33:58Z` (all UTC). Each SBOM contained 33 entries (31
external imported modules plus main module and stdlib), not all 291 selected
modules. Each JSON stream emitted the same three findings for
`golang.org/x/crypto@v0.55.0`: GO-2026-6355 and GO-2026-6354 (fixed in v0.56.0),
and GO-2026-5932 (`openpgp`, no fix). JSON-mode process status was 0 despite three
`finding` events; report the structured findings, not a clean/pass based on status.
These are the same baseline-existing module advisories from the earlier module scan.
The following compact event summary is committed here so reviewers do not need the
ignored local JSON artifacts:

| Captured UTC | Scan level and roots | JSON exit | Event result |
| --- | --- | --- | --- |
| 2026-10-01 16:30:58 | module: `sdkapiv1` | 0 | 3 module findings |
| 2026-10-01 16:32:25 | package: all four roots | 0 | same 3 findings; module/version traces only |
| 2026-10-01 16:33:58 | symbol: synthetic injected/default roots | 0 | same 3 events; no package/function trace |

| Advisory | Event summary | Module/version trace | Fixed version |
| --- | --- | --- | --- |
| GO-2026-6354 | DoS on deadlocked undecided channel in `x/crypto/ssh` | `golang.org/x/crypto@v0.55.0` | `v0.56.0` |
| GO-2026-6355 | DoS on deadlocked established channel in `x/crypto/ssh` | `golang.org/x/crypto@v0.55.0` | `v0.56.0` |
| GO-2026-5932 | `openpgp` is unmaintained/unsafe by design | `golang.org/x/crypto@v0.55.0` | none |

The JSON process status of 0 is distinct from human-mode status 3 for findings;
neither a zero exit nor an empty package/function trace means a clean SDK scan.

Package-mode traces contain only module/version, with no affected package path.
Imported x/crypto packages were `internal/alias`, `chacha20`, `internal/poly1305`,
`chacha20poly1305`, `cryptobyte/asn1`, `cryptobyte`, and `hkdf`; `ssh` and
`openpgp` were not imported. Symbol mode emitted no package/function trace for
these findings from the synthetic roots. This is scoped scanner evidence—not a
production called-path or SDK security guarantee. The 260 selected MVS modules
outside the 31-module import SBOM are not represented in these scans. A preliminary
no-import placeholder scan (SBOM=main+stdlib) was non-covering and excluded. The
wrong-cwd Go list and initial package-name alias failures were corrected; they are
invocation errors, not passes/findings.

**Targeted license scope.** The 16 SDK import additions/upgrades versus baseline
app packages covered 92 imported package records and 193 production `GoFiles`.
Root legal files and package ancestors were read and preserved by SHA-256; full
legal-file bytes remain LOCAL-ONLY. This sanitized inventory is published here so
reviewers can inspect the scoped classifications without access to ignored files:

| Imported module/version delta | Directly read root text heading/terms |
| --- | --- |
| `cloud.google.com/go/auth@v0.20.0` | Apache License 2.0 form |
| `cloud.google.com/go/auth/oauth2adapt@v0.2.8` | Apache License 2.0 form |
| `cloud.google.com/go/compute/metadata@v0.9.0` | Apache License 2.0 form |
| `cloud.google.com/go/iam@v1.11.0` | Apache License 2.0 form |
| `cloud.google.com/go/secretmanager@v1.22.0` | Apache License 2.0 form |
| `github.com/felixge/httpsnoop@v1.1.0` | MIT permission text |
| `github.com/google/s2a-go@v0.1.9` | Apache License 2.0 form |
| `github.com/googleapis/enterprise-certificate-proxy@v0.3.17` | Apache License 2.0 form |
| `github.com/googleapis/gax-go/v2@v2.23.0` | BSD 3-clause terms |
| `go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc@v0.67.0` | Apache License 2.0 form |
| `go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp@v0.71.0` | Apache License 2.0 form |
| `golang.org/x/oauth2@v0.36.0` | BSD 3-clause terms |
| `golang.org/x/time@v0.15.0` | BSD 3-clause terms |
| `google.golang.org/api@v0.287.1` | BSD 3-clause terms |
| `google.golang.org/genproto@v0.0.0-20260319201613-d00831a3d3e7` | Apache License 2.0 form |
| `google.golang.org/genproto/googleapis/api@v0.0.0-20260630182238-925bb5da69e7` | Apache License 2.0 form |

The imported `google.golang.org/api/internal/third_party/uritemplates` package
has a **separate nested `LICENSE`** (2013 Joshua Tacoma copyright, three
redistribution conditions, non-endorsement term and disclaimer; SHA-256
`fc0a2f71df4e8f047902da53d1f85301be43e0f360fc167057a2d04658ed2ba9`). Do not
collapse it into the `google.golang.org/api` module-root BSD text (Google Inc.
heading; SHA-256 `110244b02140866ee37d17fa7449436a377ec3b85a481fbb208f4c87964382de`).
These are observed text terms, not legal clearance or SPDX expressions.

The scoped ancestry inventory found 17 module-root legal files (including
`x/time/PATENTS`), that one package-ancestor LICENSE, and no NOTICE files in these
16 imported module/package ancestry paths. Among the 193 imported GoFiles, 23
leading-header SPDX statements were `Apache-2.0`, all in otelgrpc/otelhttp; no
SPDX line appeared in the other scanned headers or 18 legal files. This covers
only those imported package files/ancestors—not tests, unimported packages or all
291 selected module trees. No family classifier was used.

Structured evidence (graphs, modfiles/sums, import comparisons, scan JSON event
summaries, imported GoFile hashes/headers and full legal-file copies) remains
LOCAL-ONLY, ignored and unpublished at
`tmp/gcp-secret-manager-sdk-only-completion-2026-10-01/`; GitHub reviewers cannot
access it. The committed commands, synthetic source snippets and immutable public
references are the reviewer-accessible evidence.

**SDK-only budget and cleanup.** The maximum observed combined workspace/evidence
sample before cleanup was 653,576 KiB, below 3 GiB; this is a sampled maximum, not
a guaranteed peak. The 512-MiB reserve and 3-second polling are operational
safeguards, not continuous monitoring, pre-allocation enforcement or an OS quota.
The exact owned root/pointer were validated, `find -P` found zero symlinks, and
owner-write permissions were changed only inside that root. Removal succeeded
with zero diagnostics; root and pointer absence were verified. Repository
`go.mod`/`go.sum` hashes were rechecked unchanged; prior compact evidence remains
untouched.

## Decision and next gate

**Do not add the SDK based on this audit.** Scoped apiv1 module/package scans
reported the three baseline-existing x/crypto module advisories; the synthetic
symbol scan emitted no package/function call trace. This does not cover the 260
MVS-selected modules outside the 31-module package-root SBOM or production
reachability. Package/license evidence covers only the 16 imported module versions
and their imported GoFiles/package ancestors; it is not full-graph legal clearance.
Any further audit needs a new bounded approval. SDK adoption remains a separate
decision; no adapter, live cloud, identity, IAM, key or runtime change is authorized
here. The authorized PR is documentation-only; it does not adopt the SDK or change
application dependencies/runtime. Merge requires separate explicit owner approval.

References: [spec 34](34-gcp-secret-manager-sdk-contract-review.md),
[pinned module metadata](https://proxy.golang.org/cloud.google.com/go/secretmanager/@v/v1.22.0.mod),
[immutable source commit](https://github.com/googleapis/google-cloud-go/tree/8a17bee208939e0166936a59675414439c47e341/secretmanager).
