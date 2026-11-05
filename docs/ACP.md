# Agent Client Protocol (local preview)

`dashy acp` exposes a client-independent ACP agent over stdin/stdout. It does not
contain an editor integration or depend on a particular editor. The old one-shot
`dashy -p` mode and its flags have been removed; TUI and helper commands remain.

## Compatibility contract

| Item | Supported / tested |
| --- | --- |
| Protocol | ACP **v1**, newline-delimited JSON-RPC 2.0 |
| Schema | `agentclientprotocol/agent-client-protocol` revision `734309dc6ff8f5a3b93adca62826dd63cff85a26`, `schema/v1/schema.json` |
| Independent test peer | `github.com/coder/acp-go-sdk` **v0.13.5**, revision `0845a3bb9eddda5bfc22a94dd3598c90cb842451` (tests only) |
| Request flow | `initialize`, `session/new`, `session/load`, `session/resume`, `session/list`, `session/close`, `session/prompt`, `session/set_config_option` |
| Cancellation | `session/cancel` notification; `$/cancel_request` for individual requests |
| Authentication | Existing Dashy provider settings / environment; no interactive ACP auth methods |
| Execution policy | No approvals, matching the trusted local TUI policy |
| Input | Text only; all blocks validated before mutating history |
| Updates | Committed agent text, structured tools, terminal tool results, configuration changes, context usage |
| Configuration | Session-local model/provider and reasoning effort selectors |
| Unsupported | ACP v2, MCP, additional roots, images/audio/resources, mode configuration, client filesystem/terminals |

There is no client-specific runtime code, test fixture, configuration, or dependency
in the implementation. Interoperability tests use an independent ACP client,
isolated state and a fake provider, without paid calls or live configuration edits.

The maintained Go SDK was evaluated. Its v0.13.5 transport logs malformed raw frames,
does not return standard parse errors, and overwrites duplicate in-flight IDs.
A small internal transport enforces Dashy's framing/resource policy. The SDK is
used only as an independent ACP client and type-level compatibility test.

## Launch and setup

Build with `make install`, then configure a provider/model in the Dashy TUI. Configure
an ACP v1 client to launch the installed binary with arguments `["acp"]` in a local
workspace. No TCP listener or remote service is started by Dashy.

Use `dashy acp --no-global` to skip global instructions, or `--config FILE` for an
alternate project instruction file. Both global and project instructions load by
default. Executable blocks use `DASHY_MODE=acp`; instruction snapshots remain stored.

Clients must send an absolute `cwd` and `mcpServers: []` to `session/new`. Disable
client-side MCP injection: nonempty MCP declarations and additional roots are
explicitly rejected. Other unsupported methods return method-not-found.

Version negotiation returns protocol 1, even for unsupported requested versions,
as specified by v1. Session work is rejected until successful v1 initialization.
Optional capabilities are omitted or false. No authentication methods are advertised;
`authenticate` rejects requests. Missing setup and HTTP 401/403 return `-32000`.
Other provider failures return errors with safe, nonempty diagnostics.

Provider refusal text remains in history and is delivered as committed text with
`end_turn`, rather than claiming ACP's context-discarding refusal semantics.

## Model and reasoning selectors

`session/new` returns standard select `configOptions`: `model` with category
`model`, and `effort` with category `thought_level`. Model choices come from all
configured providers, with a five-second discovery deadline and a 2,000-choice
bound. The selected model remains available even when discovery fails. IDs encode
the provider as well as the model to avoid ambiguity across providers.

Use `session/set_config_option` to choose an offered model ID or effort value.
The response and `config_option_update` notification include the complete current
configuration. Changes are allowed between turns, are saved with session history,
and never change another session or shared TUI defaults. The effort choices match
the TUI; providers may reject levels unsupported by a particular model. `default`
means omit the reasoning effort and use the provider's default.

## History and context usage

`initialize` advertises `loadSession: true`. Clients can reconnect with
`session/load` using the saved `sessionId`, its original absolute `cwd`, and an
empty `mcpServers` array. Dashy restores the stored instruction snapshot,
conversation and model/effort selection. Before replying, it replays user and
agent messages, saved reasoning and tool calls/results through `session/update`.
Tools and instruction blocks are not re-executed. Loading an already open idle
session also replays its history; loading a busy session or a different workspace
is rejected. Clients should replace their transcript with the replay rather than
append it to a previously rendered copy.

`sessionCapabilities` also advertises `resume`, `list` and `close`.
`session/resume` restores the same state as `session/load` (`mcpServers` may be
omitted) but sends no transcript, only `usage_update`; clients keep their own copy.
`session/list` returns saved sessions newest first, optionally filtered by absolute
`cwd`, 50 per page with an opaque `nextCursor`; titles are the custom name or the
first user line. `session/close` cancels a running turn (its prompt returns
`cancelled`), waits for the session to become idle, then releases its lock, so it
can be loaded again and stops counting toward the 16-session limit.

`usage_update` reports context occupancy on session creation/load, configuration
changes and after each prompt, including interrupted turns. `used` is the latest
provider-reported input token count, or a history estimate when no count is known.
`size` uses the selected model's positive `context_length` or `context_window`
from the provider's `/models` response. When neither is available, the display
falls back to **1,000,000 tokens**. This fallback is a UI assumption, not a verified
model limit, and does not change requests or enforce/truncate the context.
No cost or token billing totals are fabricated.

## Trust and lifecycle

Commands, instruction blocks and task helpers execute freely, just as in the TUI.
This explicitly supersedes the original plan's approval gate. There are no permission
prompts or OS sandbox: subprocesses have your filesystem/network access and inherit
your environment. Review untrusted `DASHY.md` blocks before opening a project.

Each session owns its cwd, agent, model selection, SQLite lock, shell and notification
socket. No process-wide `chdir` occurs. The adapter reuses the agent runner directly:
the TUI controller's idle auto-delivery/steering would create unsolicited ACP turns.
Overlapping prompts are rejected. Helper notices/images reach their owning session;
while idle they wait for the next prompt instead of starting an unsolicited turn.

Turn cancellation stops its HTTP request and foreground process group. Explicit
background tasks survive a cancelled turn. EOF/disconnect or SIGINT/SIGTERM stops
all owned tasks, saves recoverable history, and releases sessions and sockets.

## Output and resource policy

- Publish text only after each provider response commits, once per message. No
  provisional reasoning or retry rollback is forwarded. This preview streams
  responses, not individual tokens.
- Tools have stable session-local IDs and pending/running/completed/failed states,
  raw command input, bounded output, exit code and duration. No arbitrary shell
  side effects are presented as verified file edits or diffs.
- Tool text output is capped at 64 KiB after the shell's own configured output bound.
  Configured provider credentials are redacted from errors and tool/text updates.
- Frame limit: 8 MiB; request IDs: strings or signed 64-bit integers. Maximum 64
  concurrent requests, 16 sessions, and 64 queued output frames per connection.
- One writer preserves update/response ordering. Live-update queue overflow fails
  the connection instead of dropping critical events; bulk history replay and
  responses apply backpressure. A stalled write times out after 5 seconds.
  Standard descriptors are made pollable so cancellation can interrupt stdio reads.
- Invalid JSON/envelopes/params return standard JSON-RPC errors. Unknown notifications
  and valid unsolicited responses are ignored. Duplicate in-flight IDs or oversized
  frames disconnect. Complete final JSON without a newline is accepted; truncated
  JSON is rejected. EOF cancels unfinished turns rather than draining them.
- Each prompt completes once after its queued updates. Session cancellation returns
  `cancelled`; request cancellation returns `-32800`; success returns `end_turn`.
- Stdout contains protocol frames only. Diagnostics use stderr. macOS/Linux local
  workspaces are supported; Windows, remote files and unsaved buffers are not promised.

`make test` covers fake-provider behavior under `-race`, an independent SDK client,
isolation, tools, cancellation, redaction, protocol errors, backpressure, fragmented/
oversized frames, EOF, actual binary signal handling, load/resume, list paging and
close. Richer input and additional configuration remain follow-up milestones.
