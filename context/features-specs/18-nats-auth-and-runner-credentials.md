Read `CLAUDE.md` before starting

We're turning on NATS authentication and giving each runner credentials that
can publish its own session's events and nothing else (Unit M5.5a). It is the
last thing standing between a runner and the event stream, and the first half
of M5.5.

## Why this comes first, and alone

M5.3 built the stream and the ingestor; M5.4a bound runners to their sessions
**at the ingestor** — an event whose producer is not a live runner of its
session is quarantined as `runner_not_bound`. It deliberately did not scope
NATS itself, because in M5.4a no runner connected to NATS. The tracker gates
that on the unit where a runner first publishes. That is M5.5, so it comes
first.

It is its own unit because it touches every NATS client — the API's
readiness probe, the ingestor, the runner manager, the tests and now the
runner — and because it is a credential system: key generation, issuance,
expiry and revocation. M5.4a's lesson was to split before building, not
halfway through.

**Today anything that can reach port 4222 can publish to any session's
subject, and read the whole stream.** Locally that is a laptop; in any
deployed environment it would be every process on the network path. The
ingestor's binding check limits what such a publisher can get *stored*; it
does nothing to stop it reading other sessions' events off the stream, or
flooding it. This unit closes both at the broker.

## What this unit is, and what it is not

**Is:** NATS authentication turned on, locally and in the model production
will use; a service identity for each process that talks to NATS, holding only
the permissions it needs; per-runner credentials minted at provisioning,
scoped to one session's subject, expiring; delivery of those credentials to
the runner without leaving them in its environment; and tests proving the
broker refuses what the ingestor used to be the only thing refusing.

**Is not:** the fake provider or any event a runner sends — that is M5.5b. A
runner in this unit connects, proves its credentials work, and holds them for
M5.5b to use. No change to the event contract or the ingestor's decisions.

## Decided here: decentralized JWT authentication

NATS offers static users in the server config, auth callout to an external
service, and decentralized JWT authentication with an operator, accounts and
user JWTs. **Decentralized JWT**, because it is the only one of the three
where a new user with its own permissions can be issued **without touching
the server**: the runner manager signs a user JWT with an account signing key
and hands it over, and the server verifies the signature. Static users need a
config reload per runner; auth callout puts a service we would write and
operate in front of every connection.

- **One operator, one application account, one system account.** Every
  Weave process and every runner is a user in the application account. The
  operator and system-account keys are used only to set up the server.
- **The runner manager holds the account signing key** — never the account's
  identity key, so the signing key can be rotated without reissuing the
  account. It is configuration, read from a path like the GitHub App key, and
  never database content. No other process holds a signing key.
- **Local development** generates the operator, accounts and service
  credentials once, into a gitignored directory, with a `make` target that
  `make setup` and `make dev` run when they are missing. Generated, not
  committed: a committed seed is a secret in the history, and GitGuardian
  already failed one PR over a fake one.
- Record the decision and its reasoning as an ADR — the architecture requires
  one for changes to the NATS transport's use, and the credential model is
  exactly that.

Verify the NATS server version in `infra/docker-compose.yml` supports what is
needed, and read the current `nats.go` and `nats-io/jwt` documentation rather
than working from memory. Adding `nats-io/jwt/v2` is a new dependency: see
Dependencies.

## Service identities, each with only what it needs

Write the permission set for each before configuring it, and keep them
**allow-lists**:

- **Ingestor:** the JetStream API calls to manage its stream and consumer,
  and consuming from it. It publishes nothing to session subjects.
- **Runner manager:** read-only stream information (M5.5b's drain check
  needs per-subject message counts), and nothing else on NATS. It mints
  runner credentials; it does not use them.
- **API:** a connection for the readiness probe, and nothing else. It
  currently holds a NATS connection only to probe it; say so, and give it no
  subject permissions.
- **Tests:** their own identity, with what the integration harness needs.
  Tests must not run as a service identity, so a leaked test credential is
  never a production one.

## Runner credentials

Minted by the runner manager at provisioning, one per runner:

- **Publish** to `weave.session.<its session>.events` and nothing else.
- **Subscribe** to nothing — except the reply inbox JetStream's publish
  acknowledgement needs. A plain `js.Publish` waits for the stream's ack on a
  reply subject, so a runner that may subscribe to nothing cannot confirm a
  publish. Use a custom inbox prefix unique to the runner, and allow only
  that. Get this wrong in either direction and it fails silently: too narrow
  and publishes time out, too wide and the runner can read other replies.
- **Expiry** bounded by the session's maximum lifetime, stated as a
  constant. A credential that outlives its runner is a credential for a
  sandbox that no longer exists — harmless if the sandbox is gone, which is
  exactly what teardown guarantees, but there is no reason to leave it valid.
- **Revocation at teardown:** decentralized JWT revocation means updating
  the account JWT on the server. Decide whether this unit does that or relies
  on expiry plus teardown plus the ingestor's binding, and **record which and
  why**. Either is defensible; leaving it unstated is not.
- **Bound to the runner id** as the user's name, so a server log line names
  the runner, and nothing in the credential names a customer or repository.

**Delivery** follows the git token's path from M5.4a: passed at start, never
baked into an image, and moved out of the runner's initial environment before
anything else runs, so it never sits in `/proc/<pid>/environ`. M5.4a found the
first version of that wrong with a test; reuse the mechanism, and test it
again for this credential.

The dev backend is not a boundary, and `docker inspect` on the host shows a
container's start environment. That is acceptable for a development backend
on the developer's machine, as it was for the git token; ADR-013 requires the
production backend to inject secrets without it.

## The runner, in this unit

It connects to NATS with its credentials at start and **verifies** they do
what they should: its own subject is publishable (a publish with no event,
or a JetStream-level check that stores nothing in history) and a neighbouring
session's subject is refused. A runner whose credentials do not work fails to
become ready, with a distinct exit code, rather than becoming ready and
failing later. It holds the connection for M5.5b to use.

## Everything else that connects

Every existing NATS client gets credentials: the API's probe, the ingestor,
the integration harness. An **unauthenticated connection is refused** — test
it, because the failure mode of a half-done change is a server that accepts
both, and looks finished.

## Tests

- An unauthenticated connection is refused.
- A runner's credentials publish to its own session's subject and are
  **refused by the broker** for another session's subject — the half of
  M5.3's gate M5.4a could not close.
- A runner's credentials cannot subscribe to the event stream or to another
  runner's inbox.
- An expired credential is refused.
- A runner whose credentials are wrong does not become ready.
- The credential is not in the runner's `/proc/1/environ`.
- Each service identity is refused an operation outside its allow-list — at
  least: the ingestor cannot publish to a session subject, the API cannot
  read the stream.
- The existing M5.3 and M5.4a integration tests pass against an
  authenticated server, unchanged in what they assert.

## Dependencies

`nats-io/jwt/v2` (and `nkeys`, already indirect) are needed to sign
credentials. **`go get` has dropped the `toolchain` directive twice**, each
time reintroducing standard-library CVEs `govulncheck` flags. Check `go.mod`
after adding them.

## Carried forward, and worth re-reading before starting

**From M5.4a, on secrets in a sandbox.** `os.Unsetenv` does not remove a value
from `/proc/<pid>/environ`. The runner re-executes itself with a clean
environment; this credential takes the same path.

**From M3.1 and ADR-005, on signing keys.** A private key is configuration,
read from a path, never database content, never logged.

**From PR #19, on secret scanners.** A seed or JWT in a test fixture will fail
GitGuardian, as a fake token prefix already did. Generate test credentials at
test time.

**From M5.2 and M5.4a, on test interference.** A running `make dev` claimed
the integration tests' work twice. Test credentials and stream names must not
let a dev process and a test see each other's traffic.

### Check when done

- NATS refuses unauthenticated connections, locally and in the model
  production will use.
- A runner can publish its own session's events and is refused, by the
  broker, for any other session.
- Every service identity has an allow-list, written down, and is refused
  outside it.
- The credential model, the revocation decision and the service permission
  sets are recorded — the model as an ADR, the rest in the tracker.
- `make dev` works from a clean checkout with no manual key generation.
- `go.mod` keeps its `toolchain` directive; `govulncheck` is clean.
- `make ci` and `make test-integration` pass.
