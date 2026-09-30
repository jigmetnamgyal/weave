# Claude Code stream protocol evidence (M6.1a)

## Sources and provenance

Fetched official documentation on **2026-09-30**, without invoking a model or
reading the operator's Claude authentication. These pages are unversioned;
SHA-256 identifies the fetched markdown, not a promised stable protocol version.
The operator's installed CLI reports 2.1.285, but no captured live CLI stream is
claimed. The offline decoder still needs opt-in runtime acceptance in M6.1b.

| Official source                                                                             | SHA-256 of fetched markdown                                      |
| ------------------------------------------------------------------------------------------- | ---------------------------------------------------------------- |
| [Headless CLI](https://code.claude.com/docs/en/headless.md)                                 | f2d9f93dfe887cf817912816bcb6fa39483834e42bca9fd10a0c2ec9d7e4d691 |
| [TypeScript SDK message reference](https://code.claude.com/docs/en/agent-sdk/typescript.md) | 21b0e716c2d30d4bfe598209f4c1270ceacbfac310354b21ea02853ced066fae |
| [Streaming output](https://code.claude.com/docs/en/agent-sdk/streaming-output.md)           | 3a43fda26f50c2b62bd7827699bb0542bf40df8556499d08443fee4b0de2d19d |

Documents were inspected locally under ignored `tmp/claude-protocol/`; the
repository does not vendor the entire documentation or install an SDK dependency.
The platform.claude.com SDK URL redirected to an overview HTML page, not the
reference; the evidence above is from the actual code.claude.com markdown pages.

## What the documents establish

- Headless CLI `--output-format stream-json --verbose` produces newline-delimited
  JSON. `--include-partial-messages` adds raw API stream events. The final line
  is a `result`. Failure can be a result on stdout; stdout closing alone is not
  a successful run. CLI exit status must also be checked by runtime wiring.
- A complete assistant message is emitted per nonempty content block, before
  its block-stop stream event. Several records can share `message.id`.
  `SDKAssistantMessage` has its own outer UUID, a provider session identity,
  nested message content, optional `error` and optional `aborted: true`.
- `SDKPartialAssistantMessage` is `type: stream_event` with nested raw API events.
  Stream text deltas are not an additional completed assistant message.
- Result has `subtype`, `is_error`, usage and permission denials. Documented error
  subtypes are `error_max_turns`, `error_during_execution`, `error_max_budget_usd`
  and `error_max_structured_output_retries`. Even the success arm has a boolean
  `is_error`; it must not be ignored.
- Usage includes input/output, cache-read and cache-creation input counts.
  The decoder's `agent.Usage.InputTokens` is explicitly their input sum, not a
  pricing or dollar-cost estimate. It does not add per-model usage a second time.

## Supported profile, not the entire SDK

`services/runner/agent/claudestream` supports a bounded, single-turn offline
profile. Complete assistant text becomes existing `message.created` payloads;
partial events and result text are ignored to prevent duplication. Tools and
thinking are not advertised as normalized tool/approval events. Known system,
user and stream-event records are ignored after bounded envelope decoding;
unknown top-level kinds are refused rather than silently declaring success.

The decoder requires a consistent provider session identity, a newline after
every record, one terminal result and EOF after it. A new record kind or a
multi-turn/background-result feature requires deliberate spec changes and tests.
A replay of the same assistant UUID must have the same raw record bytes;
conflicting content is refused. Normalized message IDs are deterministic UUIDs
scoped to the Weave session and outer assistant UUID, not the shared API ID.

Provider failure descriptions and decoder errors are fixed safe strings. Raw
provider errors, tool inputs, paths, user echoes, startup configuration and
unknown metadata are not persisted or logged. Assistant error/abort, a failed
result or permission denials cannot become success. Callback errors are not
reflected. An already emitted event is not rolled back if later input is refused;
future runtime wiring must convert decode failure to a failed run.

Bounds: 256 KiB per record including newline, 65,536 records, 4,096 distinct
assistant UUIDs, the existing domain text limits and 2 KiB envelope headroom
against the 64 KiB event limit. Token counts must be nonnegative integers and
must not overflow. All errors return an unusable result plus a safe category.

The synchronous decoder checks cancellation between reads, including at EOF.
It cannot cancel an arbitrary blocking reader; runtime owns closing pipes,
killing/reaping the process tree and resolving terminal-result/exit precedence.

## Verification and remaining gates

`testdata/*.ndjson` is **synthetic**, not captured from Claude, and contains only
invented text/paths. Tests include complete blocks sharing API IDs, deltas,
replays, malformed/oversized/truncated input, unknown kinds, result/error
precedence, token bounds, encoded event size, memory bounds and cancellation.
Emitted events are checked through the actual domain envelope decoder.

No Claude adapter is registered: `agent.New(ProviderClaudeCode)` still fails.
Before runtime activation: pin and verify CLI distribution/authentication,
resolve credential isolation and task/agent input delivery, verify repository
configuration cannot execute hooks/plugins/MCP, choose a reviewed tool policy,
and perform an explicitly authorized sandbox live run with verified cleanup.
See spec `context/features-specs/26-claude-code-adapter.md`.
