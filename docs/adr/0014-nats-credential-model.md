# ADR-014: NATS credentials — decentralized JWT, per-service allow-lists, per-runner scope

- Status: Accepted
- Date: 2026-09-29 (Unit M5.5a)

## Context

Until M5.5a the event broker ran without authentication. Anything that could
reach it could publish to any session's subject and read every session's
events off the stream. M5.3's ingestor limited what such a publisher could
get _stored_. M5.4a bound runners to their sessions at the ingestor. Neither
stopped reading, flooding, or a runner publishing as another session's
producer — the ingestor refuses that event, but only after the broker has
carried it.

M5.5b's runner is the first thing that publishes from inside a sandbox
running untrusted work, so the broker has to enforce scope itself.

## Decision

### Decentralized JWT authentication

NATS offers three models. Static users in the server configuration would
need a configuration reload for every runner. Auth callout would put a
service we write and operate in front of every connection. **Decentralized
JWT** lets a new user with its own permissions be issued **without touching
the server**: the issuer signs a user JWT, and the server verifies the
signature against the account it trusts.

- **One operator, one system account, one application account.** Every Weave
  process and every runner is a user in the application account. The
  operator and system-account keys are used only to configure the server.
- **The application account has a separate signing key**, held only by the
  runner manager. Users are signed with it, never with the account's identity
  key, so it can be rotated without reissuing the account. It is
  configuration, read from a path, never database content and never logged
  (the ADR-005 rule for the GitHub App key).
- **Local development generates everything** with `services/nats-setup`
  into a gitignored directory, run by `make up` when missing. Nothing is
  committed. Production keys are created and held in a secret store (M9); the
  model is the same.

### Every service identity is an allow-list

Written in `internal/adapters/natsauth` in one place, and each is tested for
both what it may do and something it may not:

| Identity       | May                                                                       | May not (tested)                            |
| -------------- | ------------------------------------------------------------------------- | ------------------------------------------- |
| Ingestor       | its stream's JetStream API, consumers and acknowledgements; reply inboxes | publish a session event                     |
| Runner manager | its stream's information (M5.5b's drain check)                            | publish a session event                     |
| API            | connect (the readiness probe is a protocol-level round trip)              | call the JetStream API, subscribe to events |
| Tests          | anything — their own identity, never a service's                          | —                                           |

An identity with no permissions is given an explicit deny-all, because an
empty allow-list means "unrestricted" to the server.

### Runner credentials

Minted by the runner manager at provisioning, one per runner:

- **publish** only `weave.session.<its session>.events`;
- **subscribe** only its own reply-inbox prefix. JetStream acknowledges a
  publish on a reply subject, so a runner allowed to subscribe to nothing
  could never confirm one, and the default `_INBOX.>` would let it read other
  clients' replies;
- **expire** after twelve hours, longer than any session this system runs;
- **named for the runner**, never for a customer, workspace or repository;
- **delivered** like the git token (M5.4a): at start, never in an image, and
  moved out of the runner's initial environment by a self re-exec before
  anything else runs, so it is never in `/proc/<pid>/environ`.

The runner **proves the scope at start**, in both directions: a JetStream
publish to its own subject that the stream refuses on sequence (so nothing is
stored, and the refusal proves the publish was permitted, reached the stream,
and the reply reached its inbox), and a publish to another session's subject
that the broker must refuse. A credential failing either check keeps the
runner from becoming ready.

### No active revocation

Revoking a user under decentralized JWT means publishing an updated account
JWT with a revocation entry. **This is not done.** A runner's credential stops
being useful through three things already in place:

1. **Teardown destroys the sandbox holding it.** ADR-013's no-retention rule
   means nothing of the runner survives teardown, the credential included.
2. **The ingestor refuses events from a runner not bound to a live session**
   (M5.4a), so a credential that escaped cannot write history after its
   runner ends.
3. **It expires** after twelve hours.

Active revocation would add a signing path to the operator's account key —
which today is used only for setup and could stay offline — and a
propagation step whose failure modes would need their own design, to close a
window that the first two already close for writes. It is the right addition
if a credential could be exfiltrated from a _live_ runner by something with a
reason to keep publishing — which is M6's threat, once a real agent runs.

## Consequences

**Accepted:**

- Every NATS client needs a credential. In development and test each defaults
  to its generated file; anywhere else an unset credential fails startup, so
  a deployment cannot silently connect unauthenticated or with a developer's
  key.
- Turning authentication on is disruptive to a running local stack: processes
  started before it hold no credentials. `make dev` regenerates and restarts.
- The escaped-credential read window: until expiry a leaked runner credential
  can still _publish_ its own session's subject. Those events are refused by
  the ingestor once the runner is torn down.

**Revisit if:** a real provider (M6) makes exfiltration from a live runner
plausible — add revocation; or the runner manager's signing key must be
rotated — the account JWT gains the new key before the old is removed.
