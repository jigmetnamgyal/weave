Read `CLAUDE.md` before starting.

# M6.1 — Claude Code adapter

**Status: planning in progress on `m6.1-claude-code-adapter`; no real adapter implemented or enabled.**

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

## Next

Verify the current official stream protocol and implement M6.1a with offline
fixtures/tests. Resolve authentication, credential isolation and tool policy
before enabling M6.1b; do not treat the local operator's CLI installation as a
production integration.
