Read `CLAUDE.md` before starting.

# M5.4d.1 — Guarded upstream transport

Implements the transport boundary specified before coding in ADR-017 and the
M5.4d parent spec (`22-workspace-egress-allowlist.md`).

## Outcome and scope

A standalone `internal/adapters/guardedhttp.Client` fetches from a previously
authorized HTTPS origin without letting its DNS answers target forbidden IPs.
No service uses it yet. The client does not authenticate or authorize hosts;
those checks remain mandatory in M5.4d.2/.3.

On each new connection, resolve once with a 10-second aggregate DNS/dial budget,
validate every answer, and dial only numeric addresses from that answer set.
Reject the whole mixed public/forbidden result. Normalize mapped IPv4 and test
it against IPv4 restrictions. Fallback candidates are checked before any dial.
TLS verifies the original hostname. Reused connections stay at their validated
origin; new connections repeat the guard.

Only exact lowercase ASCII hostnames over HTTPS/443 are supported. Reject raw
IPs, local/internal suffixes, credentials in URLs, fragments, conflicting Host,
CONNECT/TRACE and upgrades. Return redirects without following them. Never
use an environment proxy or a custom TLS dialer bypassing the guard.

## Address policy

The static table was checked against the IANA IPv4/IPv6 special-purpose registries
on 2026-09-30. Conservatively deny special-use ranges, including globally numbered
special-purpose exceptions; also deny the Azure platform virtual IP. IPv6 supports
ordinary `2000::/3` public unicast excluding protocol assignments, documentation,
6to4 and special service ranges. NAT64, ULA, link-local, multicast, unspecified,
scoped addresses and non-supported IPv6 space are refused. Public mapped IPv4
is normalized and dialed as IPv4. `IsGlobalUnicast` alone is not sufficient.
No runtime downloads or automatic address-policy updates. Table changes require
review and boundary tests. Reserved Weave hostnames need additional domain-level
policy validation in the later service; IP classification is not a replacement.

## Resource and error behavior

10-second TLS handshake timeout; 30-second response-header and idle timeouts;
1 MiB response-header cap; 8 connections and 2 idle connections per origin.
Transport errors return safe categories/cancellation instead of net/http's
URL-bearing error, which could contain a query credential. No request logging.
Response bodies stream under caller cancellation. The caller must close bodies
and supply service-wide capacity and inbound body limits: this adapter does not
implement a public listener or claim complete resource-abuse protection.

## Deterministic verification

Tests inject private resolver/dial seams and a trusted local TLS fixture. The
production address classifier stays enabled. Only the test dial seam maps an
already-validated numeric public destination to the fixture. No allow-private
flag or insecure production TLS setting exists.

Covered: public and forbidden IPv4/IPv6 classes and boundaries; mapped/transition
addresses; mixed DNS sets; pinned numeric targets; fallback; Host/SNI preservation;
changed DNS answers on the next connection; TLS trust/name failures; redirects;
CONNECT/TRACE, upgrades and unsolicited 101; DNS failure/empty answers/cancellation;
header timeout; safe error text; and disabled environment proxies.

Mutation checks observed failures after removing answer validation, restoring
hostname dialing, following redirects, and disabling certificate verification.

## Acceptance record

- `go test -race -count=3 -cover ./internal/adapters/guardedhttp`: pass, 91.6% coverage.
- `make lint-go`: pass.
- `make test`: pass.
- `go build ./...`: pass.
- `git diff --check`: pass.

No provider calls, purchased domain or customer fixtures were used for this unit.
No migration or runtime egress policy changed. PostgreSQL integration and live
forwarding are later-unit acceptance, not claimed here.

## Next

Review M5.4d.1. Then M5.4d.2: contract the tenant-scoped additions API, atomic audit,
idempotency, concurrent cap enforcement and immutable provisioning snapshots.
Finally M5.4d.3: authenticated egress edge, match-free runner forwarding and UI,
with full authorization/resource-limit and live forwarding acceptance.
