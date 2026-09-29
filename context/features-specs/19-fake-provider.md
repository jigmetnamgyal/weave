Read `CLAUDE.md` before starting

We're building the fake provider (Unit M5.5b): a deterministic coding agent
that runs inside the session's runner, produces events, and finishes — which
makes a session do something, end to end, for the first time.

Read `context/features-specs/18-nats-auth-and-runner-credentials.md` first.
This unit depends on M5.5a: the runner publishes with the scoped credentials
built there.

## Why a fake provider, and why it is first-class

The Session Notes require a deterministic agent adapter before a paid
provider is wired in, "so orchestration, events, approvals and UI are
verified independently". Nothing in the chain has been exercised with events
moving through it: M5.3 built the ingestor against test publishers, and
M5.4a's runner emits nothing. This unit is the first time a runner publishes,
the ingestor stores what a runner sent, and a session ends because its agent
finished rather than because nothing could run.

**First-class, not a placeholder.** It implements the same adapter contract
Claude Code will in M6, through the same runner, the same credentials and the
same events. If the fake takes a shortcut the real adapter cannot, the chain
it verifies is not the chain that will run.

## The adapter contract, on the runner side

`context/architecture.md` defines it:

```go
type CodingAgent interface {
    Capabilities(ctx context.Context) (Capabilities, error)
    Start(ctx context.Context, req StartRequest) (<-chan ProviderEvent, error)
    SendInstruction(ctx context.Context, req InstructionRequest) error
    Pause(ctx context.Context) error
    Resume(ctx context.Context) error
    Cancel(ctx context.Context) error
    CollectUsage(ctx context.Context) (Usage, error)
    Close(ctx context.Context) error
}
```

Build it in the runner, with the fake as its first implementation. The
runner translates `ProviderEvent`s into the event contract from M5.3 and
publishes them; the adapter never touches NATS. Two rules from the
architecture that the fake must not make easy to forget:

- **No feature may assume every provider supports pause, structured tool
  calls or token accounting.** The fake declares its capabilities honestly
  — whatever it does not do, it says it does not do — and the runner consults
  them rather than assuming.
- **Provider-specific fields live only in a namespaced metadata object.**
  The fake has none worth sending; the shape should still leave room.

`SendInstruction`, `Pause` and `Resume` have no caller until M6 and M7. Say
what the fake does with them, and do not build the control-plane path to
them here.

## Deterministic, and how a test chooses the outcome

The fake produces the same events for the same input, every time. Two
outcomes are needed so both paths are verified:

- **Success:** a fixed sequence of `message.created` events — narration of a
  plan it does not carry out — then it finishes.
- **Failure:** some messages, then `provider.failed` with a stable code.

Choose the outcome from something the session already has, not from a new
field or a test-only switch in production code. `agent_versions.model`
already exists and is free text for the fake (`deterministic-v1` today); a
second model name for the failure path is the smallest honest option. Record
the choice.

**It reads nothing from the repository and echoes nothing from the task.**
The fake's output is its own fixed text. A task body is untrusted input, and
an adapter that copies it into events verifies nothing a real one needs to do.

## How the workflow knows the provider finished

M5.4a established that liveness comes from the backend's status, not from
the runner's own claims. Keep that: the runner process **exits** when its
provider finishes, with a distinct exit code for success and for provider
failure, and the runner manager observes the exit through the backend — the
same shape as `AwaitReady`, heartbeating as it waits, bounded by the session's
maximum run time.

The runner's exit code is a **claim** about how the provider ended. The
events are another. Say which the session's outcome follows when they
disagree — an exit of success after a `provider.failed` event, say — and test
it.

## The race M5.3 and M5.4a left for this unit

Both recorded it: a runner's last events may still be in the stream when the
session ends. Two things now refuse them:

- teardown marks the runner `terminated`, after which the ingestor refuses
  its events as `runner_not_bound`;
- a terminal session refuses events as `session_terminal`.

So **the workflow must not tear down or end the session until the runner's
events have been ingested.** The mechanism has to rest on something the
runner cannot fake. The stream uses work-queue retention, so an unacknowledged
message is one the ingestor has not finished with, and the stream can report
message counts per subject. Once the runner has exited, a count of zero on
`weave.session.<id>.events` means everything it published has been stored or
quarantined. Wait for that, bounded, before teardown.

Decide what happens when the bound is reached with messages still pending —
the ingestor is down, say. Neither failing the session nor ending it silently
is obviously right. Record the choice.

This closes the Open Question M5.3 raised and M5.4a moved here. Do **not**
loosen the ingestor's terminal-session or binding refusals to make the race
go away. Those are invariant 10 and M5.3's gate, and ordering the workflow is
the fix.

## How a session ends

The transition table allows `running → review_ready → finalizing →
completed`. Review is M7 and delivery M8, so neither exists yet.

- **Success:** `running → review_ready`, with a reason saying the provider
  finished and review arrives later. The runner is torn down: ADR-013 keeps no
  source, and the fake changed nothing. The workflow ends. The session waits
  in `review_ready` for a unit that can review it.
- **Provider failure:** `running → failed`, with a reason naming the
  provider's failure code — our words, from the code, never the provider's
  message text.
- **Timeout:** bounded by the session's maximum run time, ending `expired`.

Say in the tracker that `review_ready` is where M5.5 leaves successful
sessions, and what M7 has to do with the ones already sitting there.

**Workflow version 4**, gated like versions 2 and 3.

## Tests

- **End to end**, through Temporal, a real runner container and a real
  JetStream: a session created as the API creates one reaches
  `review_ready`, its `session_events` hold the fake's messages in sequence
  from 1, attributed to its bound runner, with **no quarantine rows** for
  it, and the runner is torn down.
- The failure model ends `failed`, with `provider.failed` stored and the
  reason naming the code.
- **Determinism:** two sessions on the same model produce identical payload
  sequences.
- **The race, closed:** a runner that publishes its last event immediately
  before exiting has that event stored, not quarantined. Watch this fail
  with the drain wait removed; it is the test the Open Question has been
  waiting for.
- A runner cannot publish to another session's subject (M5.5a's broker-side
  refusal, exercised through a real runner).
- The adapter's declared capabilities are what the runner consults.

## Carried forward, and worth re-reading before starting

**From M5.3, on the terminal-session race and the ingestor's refusals.** They
are right, and the fix is ordering, not loosening.

**From M5.4a, on exits as the only trusted signal.** The control plane reads
a runner's exit code, never its output.

**From M5.1 and M5.4a, on teardown.** Every path out tears the runner down,
including this unit's new ones.

**From the project rules.** Never present a fake integration as production
behaviour. The fake provider is labelled as one wherever a person could see
it, starting with the session's transition reasons.

### Check when done

- A session reaches `review_ready` with the fake's events in its history,
  in order, from its bound runner, with nothing quarantined.
- The failure model ends `failed` with the provider's code named.
- A last-moment event is stored, not quarantined, and the test was watched
  failing without the drain wait.
- The outcome-selection, exit-versus-event and drain-timeout decisions are
  recorded in the tracker with reasoning.
- `make ci` and `make test-integration` pass.
