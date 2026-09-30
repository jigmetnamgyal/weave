Read `CLAUDE.md` and spec `26-claude-code-adapter.md` first.

# M6.1b.1 — Claude process supervisor

**Status: built and verified, awaiting review on `m6.1b1-claude-process`. Runtime selection remains disabled.**

## Bounded outcome

A runner-side process primitive over M6.1a's decoder, tested entirely with a
synthetic child executable. Not a registered `CodingAgent`, not a paid run, and
not completion of M6.1b. Immutable session task snapshots, credential ownership
and delivery, provider-host egress, CLI distribution in the sandbox and verified
CLI isolation remain separate activation gates.

## Invocation profile

- Require an absolute trusted executable path, expected version **2.1.285**,
  absolute existing workdir, opaque session ID, bounded model selector, API key
  and bounded UTF-8 task/instruction data. Version-probe stdout is capped and
  probe time bounded. A version string is not a binary integrity attestation.
- The current official headless docs establish bare-mode API-key authentication,
  not local subscription login. No local login or credential discovery occurs in
  this unit. The primitive accepts a trusted caller's key; **key ownership and
  retrieval are not decided or wired here**.
- Pass task and instructions as a bounded JSON document on stdin, never shell
  syntax or command-line arguments. This is data, not a privileged system prompt.
- Construct an explicit environment, never inherit `os.Environ`: API key,
  fresh private HOME/TMP/config directory, fixed PATH and locale, and disable
  nonessential traffic. GitHub/NATS secrets, proxy/base-URL overrides, node
  options, local provider logins and parent configuration are not forwarded.
- Request `--bare`, empty setting sources/settings, strict empty MCP configuration,
  deny all tools and empty built-in tools, `dontAsk`, no session persistence,
  print mode and verbose stream-json. No permission bypass and no interactive
  approval path. This profile cannot inspect or edit a repository.
- Temporary state is private and outside the checkout (including symlink aliases),
  removed after reaping; removal failure is a safe reported error, not silent
  success.
  Weave does not write keys or prompts into files. These flag/environment choices
  are verified against a synthetic executable, **not claimed as verified CLI
  isolation**; actual CLI behavior must be tested before activation.

## Lifecycle and event semantics

- Linux/macOS process groups, bounded run/probe contexts, group kill on context
  cancellation, decode/delivery refusal or teardown; reap the direct child.
  Observe exit without reaping (Linux `waitid(WNOWAIT)`, macOS kqueue `NOTE_EXIT`),
  join/retire all group-signaling callbacks and kill remaining group members
  while the child PID is still retained, then call `Wait`. No group signal may
  follow reaping, including in the version probe.
  Descendant termination is group-scoped, not a claim of arbitrary daemon/process
  escape containment (the sandbox owns that boundary).
- OS stdout pipe ownership stays with the supervisor, so child exit cannot close
  the read end before buffered terminal output is decoded. Stderr is discarded,
  never reflected in errors/logs. Prompt writing, waiting and event delivery must
  not create unbounded shutdown waits. Exit after stdout EOF has a two-second
  grace; every cooperative callback has a one-minute delivery deadline.
- Event callbacks receive context and **must honor it**: an arbitrary callback
  that ignores cancellation cannot be forcibly stopped safely by Go.
- Hold the decoder's final `provider.failed` until process exit is known. Emit
  exactly one final failure for a failed result, rejected/truncated stream or
  nonzero exit, after completed messages. Only valid terminal success plus zero
  exit plus uncancelled context is success. Infrastructure/start/config/delivery
  errors are fixed categories; cancellation wins and is not success.
- Return measured usage only for a validated result; no usage event or pricing
  claim. Maintain the fake adapter unchanged; `agent.New` still refuses Claude.

## Protocol/distribution evidence

Official headless and CLI-reference markdown fetched 2026-09-30:

- `https://code.claude.com/docs/en/headless.md`, SHA-256
  `f2d9f93dfe887cf817912816bcb6fa39483834e42bca9fd10a0c2ec9d7e4d691`.
- `https://code.claude.com/docs/en/cli-reference.md`, SHA-256
  `f46fbeb0d1b3acc637d74ba7b629fb71506d341d7f9d0965c269da17851d90f2`.

Registry metadata confirms `@anthropic-ai/claude-code@2.1.285` exists, with license
`SEE LICENSE IN README.md`. No package installed for this unit; redistribution,
terms/account approval and artifact integrity verification remain activation
requirements. Public docs/registry requests are not paid model invocations.

## Acceptance checks

Synthetic child tests cover version refusal, stdin-only prompt data, environment
allowlist and explicit flags, private-home cleanup, valid success/failure,
success output followed by nonzero exit, malformed output, stderr flood, hung
child, inherited-pipe descendants, backpressured/cancelled delivery and safe
error text. Test the negative cases through deliberate mutations. Full unit
suite, relevant race tests, lint and Go build must pass. No integration/session
worker or paid-provider test is needed for this isolated primitive.

## Implementation and verification record

- Added `services/runner/agent/claudeprocess`: real subprocess supervision and
  offline stream normalization, with Linux/macOS process groups and fail-closed
  validation on unsupported platforms. No adapter registration or image changes.
- Synthetic Go CLI under `testdata/fakecli`, compiled locally by tests, checks
  argv, stdin and environment; simulates nonzero exit, failed/malformed streams,
  stderr floods, hung processes, backpressure and surviving inherited-pipe
  descendants. No Claude install, model request or credential discovery.
- Cleanup, private-home placement, safe error categories and version/prompt
  bounds tested. Decoder failure emission is held until exit status is known;
  failed result/stream/exit yields exactly one final failure event.
- Race tests for runner/agent packages pass three repeated runs. Process package
  statement coverage is 89.5%. Full unit suite, lint (only existing web warnings),
  Go build, typecheck, contracts and targeted formatting pass.
- Five deliberate mutations caught: parent environment inheritance, omitting
  bare mode, prompt in argv, ignored nonzero exit and killing only the leader
  instead of its process group. The descendant test explicitly cleans its owned
  synthetic PID even when a mutation disables group termination.

## PR #33 review verification

- Fixed the valid post-wait PID/PGID reuse finding for both runtime and version
  probe. A mutex-protected signaling lease is retired before reaping; all
  cancellation callbacks join before identity release. No hidden exec-context
  watcher can bypass this fence.
- Tests cover simulated ID reuse/stale callbacks, an in-flight signal joined by
  retirement, non-reaping native exit observation and pre-reap signal ordering.
  Mutations allowing retired signals and consuming exit status early are caught.
- CI also exposed an existing Clerk startup-test race: the JWK cache begins
  fetching asynchronously with `WithWaitReady(false)`, so a zero-fetch counter
  at constructor return was not its guarantee. Replaced it with a blocked
  transport proving construction does not wait for network completion, and
  corrected comments only; authentication behavior is unchanged. The test catches
  the mutation to `WithWaitReady(true)`; Clerk race suite passes ten repeats.
- Process race tests pass three repeats, full unit suite, lint and Go build pass;
  process coverage 88.7%. Linux cross-compilation passes. A local native Linux
  Docker attempt could not complete the image pull; no test container remained.
  Native Linux runtime validation is left to current-head CI.
- Existing `golang.org/x/sys` v0.47.0 is now a direct import for Linux waitid;
  no module version or checksum changed.

## Next

Review the process primitive. Then define immutable task/agent input snapshots
and a reviewed credential-delivery boundary; verify the pinned real CLI's
repository-config isolation and terms/account approval in an explicitly opted-in
sandbox test before enabling runtime selection or any tools.
