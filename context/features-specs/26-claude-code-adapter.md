Read `CLAUDE.md` before starting.

# M6.1 — Claude Code adapter

**Status: M6.1a offline decoder built and verified, awaiting review on `m6.1-claude-code-adapter`; no runtime adapter enabled.**

## Outcome and boundary

Implement the runner-side `CodingAgent` contract for Claude Code. The control
plane does not launch a provider CLI. Events still pass through the existing
runner relay, scoped broker credentials, ingestor and halt/drain lifecycle.
M5.4d is merged as ad8491c (PR #31), with the operator's admin-screen walkthrough
confirmed. Existing Vercel staging gates remain unchanged.

This is not the whole of M6: the live session room, cursor pagination gate,
reconnect and participant-instruction transport are separate slices. Tool
approval orchestration remains M7; this adapter must not silently bypass it.

## Findings from the initial inspection

- `services/runner/agent/agent.go` defines `CodingAgent`, `StartRequest`,
  `ProviderEvent` and the fake adapter. `agent.New` currently refuses every
  provider except fake.
- `StartRequest` supplies identifiers, model and checkout directory, but no
  task text. Runtime delivery of task and versioned agent instructions needs
  an explicit bounded untrusted-input path before a real session can run.
- `services/runner/main.go` scrubs GitHub and NATS secrets before repository
  work. There is no provider credential path yet. Adding an environment key
  without considering child-process visibility would weaken that boundary.
- Locally installed Claude Code reports version **2.1.285**. Its help documents
  print mode, stream-json output/input, partial messages, no session persistence,
  tool selection, permission modes, bare mode and strict MCP configuration.
  **Help output is not a verified wire schema or a tested security guarantee.**
  No model invocation or paid request was made during this inspection.

## Delivery split

### M6.1a — protocol evidence and offline normalization

First bounded implementation slice. Before writing the decoder, obtain the
current official protocol documentation and record its version/source. Use
synthetic fixtures without customer source, prompts or credentials; label them
synthetic rather than claiming they came from a live provider.

Acceptance criteria:

- Bounded newline-delimited JSON decoding, including records split across reads,
  truncation, oversized records, malformed JSON and unexpected record kinds.
- Explicit mapping into the existing domain event payloads. Provider-specific
  fields belong only under namespaced metadata; raw provider envelopes are not
  copied into infrastructure logs or emitted wholesale.
- Text deltas and complete messages must not duplicate the same assistant text.
  Identity and ordering rules must be specified against the verified protocol.
- Usage and result/error handling follow documented semantics. An error result
  cannot become a successful run merely because stdout closes normally.
- Decoder errors are safe categories, never raw JSON, prompt or provider output.
- Table-driven tests and relevant negative mutations; no provider credentials,
  network access or paid account needed.
- `agent.New` continues to refuse Claude until runtime wiring is complete.

### M6.1b — process lifecycle and sandbox wiring

Separate spec refinement after the protocol is verified:

- Pin the CLI installation/version for the sandbox; verify its distribution and
  supported headless authentication against current official terms and docs.
- Define task/agent input delivery, prompt-size limits and immutable session
  inputs. Prompt text must not become a shell command or command-line argument.
- Define provider credential ownership, injection, child-process exposure and
  revocation. Never reuse the operator's local Claude login or send GitHub/NATS
  secrets into the provider's environment. Never write credentials to the
  repository volume or logs.
- Verify isolation from repository-supplied hooks, settings, plugins and MCP
  configuration. CLI flag names alone are not evidence of isolation.
- Explicitly choose the tool/permission policy; no blanket permission bypass
  introduced to make headless execution work. Defer approval-dependent tools
  until the approval path exists, or specify a separately reviewed safe subset.
- Cancellation kills and reaps the process tree within a bound, including under
  output backpressure. Exit status, terminal result and cancellation have tested
  precedence; stderr must not leak into default logs.
- Capabilities are honest: instruction, pause, resume and token accounting are
  enabled only when their behavior is implemented and verified.
- Wire runtime selection only after credential, input and isolation tests pass.
  Keep the fake path working. Any paid live acceptance run is opt-in, never CI,
  with cleanup of sandbox/snapshot/tunnel resources verified afterwards.

## M6.1a protocol decisions

Official documents fetched on 2026-09-30 (unversioned pages; hashes recorded in
`docs/runbooks/claude-stream-protocol.md`). They describe NDJSON, complete
assistant blocks, partial API events and a final result. Multiple assistant
records can share the API `message.id`; identity therefore uses the outer
assistant UUID, scoped to the Weave session, not that shared API ID.

- Emit only complete assistant text as `message.created`. Ignore partial events
  and the final result text, so neither duplicates completed text. Tools,
  thinking, user echoes and system metadata are not persisted by this slice.
- A stream ends successfully only with one validated final result followed by
  EOF. Unknown top-level kinds, records after result, missing result, malformed
  shapes and missing final newline are refused. This intentionally supports a
  narrow single-turn profile, not every future SDK feature.
- Preserve result token accounting in a return value, not a new `usage.updated`
  transport event: that type does not exist yet in the event contract. Input
  usage includes uncached, cache-read and cache-creation tokens, with overflow
  checks. Monetary accounting and model-specific usage are not implemented.
- Assistant errors/aborts, error result subtypes, `is_error: true` and nonempty
  permission denials cannot be successful, even if the result says success.
  Emit only stable failure codes and fixed descriptions, never provider errors.
- Bound records to 256 KiB, records per stream to 65,536, assistant identities
  to 4,096 and text to the existing domain limits plus encoded-envelope headroom.
  Equal assistant UUID/raw-record replays are absorbed; conflicting replays
  are refused. Known ignored record bodies are bounded but not interpreted.
- Bind all handled records to one nonempty provider session identity. Neither
  that identity nor provider paths, settings, tools or raw usage are metadata
  copied into emitted events. No namespaced metadata is needed in this slice.
- The synchronous decoder observes context between reads/deliveries; it cannot
  interrupt an arbitrary blocking `io.Reader`. M6.1b owns pipe closure and
  process-tree termination. Callback failures return a safe category without
  reflecting callback/provider content.

## M6.1a implementation and verification record

- `services/runner/agent/claudestream/decoder.go`: synchronous bounded NDJSON
  decoding into existing `agent.ProviderEvent` payloads and returned terminal
  usage. No CLI launch, secret access, networking or adapter registration.
- `testdata/success.ndjson` and `failure.ndjson` are labelled synthetic by the
  protocol runbook, with deliberately invented diagnostics and paths.
- Tests validate emitted events through `domain.DecodeEvent`, one-byte and short
  reads, replay/identity behavior, domain and encoded text limits, terminal and
  error semantics, count limits, safe reader/callback errors and cancellation
  including cancellation during the final EOF read.
- Race tests for runner/agent packages pass three consecutive runs. Coverage is
  96.6% for the decoder. A short fuzz run completed over 200,000 executions with
  no failure; this is a local smoke run, not an exhaustive protocol proof.
- Five deliberate mutations were caught: ignore the result's `is_error`, use
  the shared API ID for message identity, accept EOF without result, remove the
  encoded payload cap, and remove the assistant-identity memory cap.
- `make test`, `make lint` (only six pre-existing web warnings), `make build`,
  `make typecheck`, contracts check and targeted formatting pass. No database or
  broker behavior changed, so no integration/live suite was needed for this unit.

## Next

Review and merge M6.1a. Then refine M6.1b: authentication/credential ownership,
isolation from repository configuration, immutable task/agent input delivery and
tool policy before process wiring or paid acceptance. Do not treat the local
operator's CLI installation as a production integration.
