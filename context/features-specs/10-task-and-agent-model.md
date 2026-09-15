Read `CLAUDE.md` before starting

We're adding the things a session will be created *from* (Unit M4.1): a
task describing what someone wants done, and a versioned agent profile
describing what will attempt it and what that provider can do.

Nothing runs yet. This unit produces records, and the only interesting
question about a record is whether it still means the same thing a month
later when something reads it back.

## Why this is its own unit

M4's line is "task model, agent profiles, provider capabilities, and
session creation" — four tables and a state machine. M3 was split into
four units for less than that, and the seam here is clean: tasks and
agent profiles are static definitions, while session creation is a
lifecycle with a state machine, an outbox and a Temporal workflow.

So: **M4.1 is tasks, agents and agent versions. M4.2 is session
creation.** Splitting after the definitions means M4.2 can be about the
lifecycle rather than about what it operates on.

## The decision this unit turns on

**An agent profile is versioned, and a session will reference a version
rather than a profile.**

Invariants 9 and 10 say session events are append-only and terminal
session history is immutable. That is not achievable if the thing a
session ran under can be edited afterwards. Change an agent's model or
its tool permissions, and every finished session silently starts
claiming it ran under settings it never saw — history rewritten by a
form submission, with nothing recording that it happened.

So `agents` carries identity and the current pointer; `agent_versions`
carries the settings, and rows in it are **immutable once created**.
Editing a profile writes a new version. M4.2 pins a session to the
version id.

The same argument covers provider capabilities: what a provider could do
at the time a session ran is part of what that session's history means.

## Data

Three tables, all workspace-owned, all with `ENABLE`/`FORCE ROW LEVEL
SECURITY` and policies **in the migration that creates them**. RLS is off
by default, so a tenant table created without it is silently unprotected
and looks entirely correct — this has to be in the same migration, not a
follow-up.

- `tasks` — `workspace_id`, a title, a body, the repository it concerns,
  who created it, timestamps, and a status narrow enough to be useful
  (`draft`, `ready`, `archived`) rather than a free string.
- `agents` — `workspace_id`, a name, and the current version pointer.
- `agent_versions` — `agent_id`, `workspace_id`, a version number, the
  provider, the model, the declared capabilities, the tool policy, and
  `created_by`. **Append-only, enforced by trigger**, the way
  `audit_events` already is: a row trigger refusing UPDATE and DELETE and
  a statement trigger refusing TRUNCATE, because a row trigger alone
  leaves the table erasable in one statement.

## Task text is untrusted input

`context/architecture.md` is explicit that the execution plane treats
task text as untrusted. It is worth being equally explicit about the
control plane, because the mistake is available here first:

- A task body is stored and returned as data. Nothing interpolates it
  into a command, a prompt template, a branch name, a log format string
  or an audit `detail` field that something later parses.
- It is bounded in length at the database, not only in the handler, so a
  path that skips validation cannot write something unbounded.
- The API returns it unmodified. Sanitising on the way out would make the
  stored value and the returned value disagree, which is worse than
  either — the caller cannot then tell what is actually stored.

## Capabilities are declared, not assumed

`context/architecture.md`: "No feature may assume every provider supports
pause, structured tool calls, token accounting, or identical permission
semantics."

An agent version therefore declares what its provider supports, and the
declaration is a closed set of named capabilities rather than free JSON —
an unrecognised capability name is a typo that silently disables a
feature, and a closed set turns it into a rejected write.

This unit only records them. Negotiating them against a live provider is
M5, where the adapter exists. The two must agree, and the way they will
be made to agree is a test in M5 that fails when a declared capability
has no adapter support — worth stating now so it is not discovered then.

## Authorization

Membership decides visibility, permission decides the operation, as
throughout.

Tasks and agent profiles are workspace configuration, so the question is
which permission governs them. **Decide this explicitly and expect the
matrix to make you**: `internal/domain/authorization.go` is the only
place a role implies anything, and `TestMatrixIsExhaustive` fails the
build when a permission is added without deciding every role pairing,
including the denials written out as `false`.

Either reuse `workspace:manage`, or add `task:manage` and `agent:manage`
and decide all four roles for each. Reusing is defensible for a first
cut; inventing permissions nothing distinguishes is not. Whichever is
chosen, say why in the tracker.

## Tests

- A task body at the length limit stores and returns byte-identical;
  one over it is refused.
- An `agent_versions` row cannot be updated or deleted — proven against
  real PostgreSQL, including TRUNCATE, because that is a separate trigger.
- Editing a profile creates a new version and leaves the old row exactly
  as it was, byte for byte.
- An unrecognised capability name is refused rather than stored.
- Cross-tenant: a task and an agent from another workspace are absent,
  not forbidden — 404, not 403.
- The three new tables return nothing to `weave_app` with no tenant
  context, and nothing when another tenant's context names this workspace
  explicitly. Connect as `weave_app`, or the test proves nothing: the
  owner is a superuser locally and bypasses every policy.

## Carried forward, and worth reading before starting

**From M3.3, on where a test lives.** A service test asserted the right
return value and passed while the HTTP handler discarded it, so the API
answered 500 for a branch that had been created. Asserting a value one
layer below the behaviour proves the layer below and nothing about what a
caller receives. If the behaviour is "what the API returns", the test
belongs at the transport.

**From M3.3 again, on comments that defend wrong code.** A validation
case rejected a legal ref under a comment asserting it implemented a Git
rule it did not implement. The comment made it look deliberate, which is
harder to catch in review than a bare wrong condition. Where a rule is
restated from a spec, cite the rule and make the code match what was
cited.

**From M3.1, on anything that deduplicates, retries or leases:** write
down what must happen for each outcome before choosing the mechanism.
That path took three attempts, each fix trading one failure mode for
another.

### Check when done

- A task is created, listed and read back with its body unchanged.
- Editing an agent profile produces a second version, and the first is
  untouched.
- The append-only triggers refuse UPDATE, DELETE and TRUNCATE.
- A viewer cannot create either; the API refuses them.
- Cross-tenant reads return nothing, verified as `weave_app`.
- The permission decision is recorded in the tracker with its reasoning.
- `make ci` and `make test-integration` pass.
