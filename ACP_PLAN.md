# ACP implementation plan

## Agreed scope updates

- Source budget is now **10,000 production Go lines**, enforced by AGENTS.md and CI.
- The previous one-shot/headless `-p` mode is removed, per the user's request.
- ACP runs code freely, like the TUI: **no permission prompts**, explicitly requested
  by the user. This supersedes section 9's approval gate and its acceptance tests.
- Implementation is client-independent; no client-specific code or fixtures are included.
- The first preview implements ACP v1 text/tools, cancellation, isolated sessions,
  and standard session-scoped model/reasoning configuration selectors.
  Exact schema and SDK test-peer revisions and limitations are in `docs/ACP.md`.
- Core runner is reused directly; TUI controller auto-delivery does not match ACP
  prompt ownership. TUI, task helpers, saved history and instruction snapshots remain.
- ACP v1 session loading now restores persisted history and configuration and replays
  the transcript without rerunning tools. Context occupancy is reported through
  `usage_update`, with a 1,000,000-token display fallback for unknown model limits.
- `session/resume` (no replay), `session/list` and `session/close` are implemented.
- PNG/JPEG image prompts are implemented and replayed.
- MCP is out of scope by agreement. Audio/resources and client filesystem/terminal
  integration remain follow-up milestones and are not advertised.

The original roadmap below is retained for follow-up work. Explicit scope updates
above take precedence where the original roadmap conflicts with them.

## Goal and scope

Expose Dashy as an Agent Client Protocol agent that an editor can launch with
`dashy acp`. Reuse the existing agent and controller; preserve TUI, one-shot,
background task, session storage, and helper behavior.

This is agent-side ACP support. Hosting other ACP agents inside Dashy is a
separate feature. No remote server, browser transport, or ACP proxy in this plan.

## 1. Establish the protocol contract

- Choose a concrete editor and release for the first interoperability target.
  Start with Zed unless there is another preferred client.
- Check the protocol versions that release supports. Upstream currently publishes
  separate v1 and v2 schemas; do not mix their methods or lifecycle semantics.
- Pin a schema revision and SDK version, if using an SDK, in the implementation PR.
- Prefer one version for the first release. Supporting a second version requires
  its own mapping and tests, not a claim of compatibility through shared JSON-RPC.
- Record mandatory methods, optional capabilities, authentication behavior,
  cancellation behavior, and terminal states from the selected stable schema.
- Compare a maintained Go SDK with a small internal transport. Adopt the SDK if it
  provides version compatibility, bidirectional requests, cancellation, and clean
  stdio integration without bringing excessive complexity. Do not generate the
  entire schema into production source just to implement a small subset.

References:
- https://agentclientprotocol.com/
- https://github.com/agentclientprotocol/agent-client-protocol/tree/main/schema
- https://github.com/agentclientprotocol/agent-client-protocol/tree/main/docs/protocol

Version-specific examples: v1 includes `session/load`, `authenticate`, filesystem
and terminal client methods; v2 has a different authentication and prompt lifecycle.
Confirm each feature against the pinned schema before implementation.

Acceptance: a checked-in compatibility matrix identifies the exact supported
version and editor release. Unsupported capabilities are explicitly disabled.

## 2. Baseline and prerequisites

Audit observations from the current working tree:
- The agent, controller, and TUI are already separated.
- The controller exposes input, cancellation, session operations, and ordered events.
- Agent events are display-oriented; they are not a complete ACP tool-call model.
- Shell commands execute locally without a user permission handshake in the
  inspected execution path.
- `go vet ./...` and `go test -race ./...` pass.
- `make test` currently fails its formatting check on `internal/agent/timeline.go`.
- Production Go source currently totals 8,937 lines, above the 7,000-line project cap.
- There are existing uncommitted changes. Do not overwrite or include them in ACP
  commits without reviewing ownership and intent.

Tasks:
- Restore a green baseline without silently rewriting unrelated work.
- Agree on a source-budget decision before adding production code: meet the
  existing cap through focused simplification, or explicitly amend the policy.
  Do not hide excess code in generated files or compressed formatting.
- Fix empty provider stream diagnostics separately: retain a safe error type/code
  and a nonempty fallback, with tests for missing or nested provider fields.
- Preserve secret redaction in all new errors and logs.

Acceptance: known baseline issues are resolved or explicitly tracked; no unrelated
working-tree changes are lost; the source-budget policy is decided.

## 3. Architecture

Proposed flow:

    editor <-> ACP stdio transport <-> ACP adapter
                                      |
                               session registry
                                      |
                          controller + agent per session
                                      |
                        storage / shell / background tasks

- Add an isolated `internal/acp` package and an explicit CLI subcommand.
- Keep protocol types and JSON-RPC concerns out of the agent and TUI packages.
- Reuse the controller where semantics match; add narrow lifecycle hooks where
  needed rather than driving the TUI or wrapping `dashy -p` subprocess output.
- Add a runner mode for ACP if needed. Do not reuse print mode if doing so changes
  instruction loading, stdout routing, or session lifecycle unintentionally.
- Pass session cwd explicitly. Never use process-wide `os.Chdir` for separate
  sessions; it would race between concurrent sessions.
- One controller/agent owns each active session. The registry maps protocol IDs
  to owned sessions and enforces storage locks and resource limits.
- Start by testing one session, but support several session objects in the design.
  A second `session/new` must not silently replace the first session.
- Controller callbacks currently run in emission order under controller locking.
  They must enqueue events, not perform blocking client RPC or call the controller
  recursively. Define bounded buffering and fail/cancel on a stalled consumer;
  never silently drop protocol-critical events.

Acceptance: core behavior can be exercised with an in-memory ACP peer and does not
require a terminal, subprocess wrapper, or editor installation.

## 4. JSON-RPC and stdio transport

- Implement the selected version's newline-delimited JSON-RPC framing.
- Serialize all stdout writes through one writer; diagnostics go to stderr only.
- Keep the read loop running while prompts, permission requests, and tools run.
- Correlate agent-to-client request IDs independently from incoming request IDs.
- Validate JSON-RPC envelopes, required parameters, session IDs, and state.
- Return standard parse, invalid-request, invalid-params, and method-not-found
  errors as appropriate. Never reply to notifications as if they were requests.
- Handle unknown optional fields for forward compatibility.
- Set documented limits on frame size, pending RPCs, and buffered output. Avoid
  the default scanner's small token limit for image-bearing requests.
- Define handling for malformed frames, truncated final frames, duplicate in-flight
  request IDs, unexpected responses, write failures, EOF, and process signals.
- EOF or transport failure cancels work, resolves waiters, and releases sessions.

Acceptance: a fake peer can initialize, exchange requests in both directions,
interrupt a long prompt, and disconnect without deadlocks or stdout pollution.

## 5. Initialization, configuration, and authentication

- Implement version negotiation, agent identity/version, and honest capabilities.
- Reject session work before successful initialization when required by the spec.
- Record client capabilities per connection and use only negotiated features.
- Read existing provider settings and environment credentials without exposing them.
- Define headless instruction loading explicitly: project and global instructions
  should follow a documented ACP policy, with override support if needed.
- Missing credentials return the protocol's applicable authentication/setup error.
  Do not launch TUI setup or wait for interactive terminal input.
- Only advertise authentication methods that are implemented; do not advertise
  browser login merely because credentials can be supplied through the environment.
- Keep model/effort overrides session-scoped. An editor change must not unexpectedly
  overwrite global settings or alter another active session.

Acceptance: configured and unconfigured launches behave predictably, secrets never
appear in RPC responses, and capability declarations match actual behavior.

## 6. Session lifecycle

- Create a session with validated cwd, isolated turn state, and stable ID.
- Reject invalid cwd and unknown sessions with appropriate protocol errors.
- Decide how external protocol IDs map to persisted Dashy session IDs.
- Define session creation parameters that the selected version requires, including
  MCP server declarations. Unsupported requested integrations must not be silently
  accepted as operational; document the initial limitation.
- Preserve the existing per-session process lock; reject double ownership.
- Implement load/resume only when advertised and according to that version's
  history replay requirements. Replay must not execute tools again.
- Keep instruction snapshots and existing saved history semantics intact.
- Reject overlapping prompt turns for a session if required. Do not automatically
  treat a second ACP prompt as the controller's TUI steering input.
- Allow independent sessions to progress without cross-session input or cancellation.
- Define cleanup for close, connection loss, cancelled creation, and storage errors.

Acceptance: create two sessions in different directories, interleave work, cancel
one, and prove that cwd, history, provider choices, and tasks remain isolated.

## 7. Prompt lifecycle and content

- Support text content first; validate all content blocks before mutating history.
- Add images only after format, MIME type, size, and persistence limits are tested.
- Explicitly reject unsupported content; never quietly discard attachments.
- Map accepted prompt input to the existing agent input representation.
- Translate assistant streaming into protocol message chunks. Existing final
  events can repeat the full text already streamed: avoid duplicate editor output.
- Handle provisional deltas and retries carefully. ACP clients may not be able to
  retract text already displayed. Choose and test a version-compatible strategy
  rather than forwarding the TUI rollback behavior blindly.
- Emit thought/reasoning content only when intentionally exposed and supported;
  never expose hidden provider state or credentials through generic event dumping.
- Map success, cancellation, refusal, and failure to the selected protocol's exact
  result/error forms. Do not invent a universal stop reason for provider errors.
- Complete a prompt exactly once, after its required updates have been written.
- Route task-completion notices and helper-delivered images to the correct session;
  distinguish them from user prompt requests and unsolicited new turns.

Acceptance: a text-only turn streams once, a tool turn resumes correctly, retries
produce documented output, and no prompt hangs after cancellation or provider failure.

## 8. Structured tool events

- Add a small structured execution lifecycle to the core where display events
  lack tool identity, status, input, output, or execution timing.
- Give every call a stable ID and emit pending/running/completed/failed states in
  the selected schema's representation.
- Map command input and bounded output without parsing rendered TUI strings.
- Preserve request/output association across retries, cancellation, and resume.
- Report nonzero exit status, timeout, denied permission, and cancellation distinctly.
- Include file locations or diffs only when known reliably; do not infer arbitrary
  shell side effects from command text and present them as verified edits.

Acceptance: each visible tool call reaches one terminal state and the editor can
correlate its input and output without duplicate or orphaned tool entries.

## 9. Permissions and execution policy

- Add an asynchronous permission hook before shell execution, shared by all paths
  that can start commands, including background-task creation through helpers.
- Treat arbitrary Bash as arbitrary code execution. Reliable read-only/write-only
  classification cannot be achieved with a few string checks.
- Default ACP policy: ask before execution or deny when approval is unavailable.
  Any fully trusted mode must be an explicit, documented opt-in.
- Map approval choices and cancellation to the selected protocol's permission API.
- Scope remembered grants to the advertised session/tool policy; never persist a
  broad approval accidentally across projects or client connections.
- Waiting for approval must not hold controller locks or block transport reading.
- Timeout, disconnect, malformed replies, and cancelled prompts deny pending work.
- State clearly that permission prompts are not an OS sandbox. Document inherited
  environment and network access; avoid sending credentials to the editor in logs.
- Keep the existing TUI/print behavior unchanged unless a separate change is agreed.

Acceptance: denial prevents process creation; cancellation during approval unblocks
both sides; no alternate task-launch route bypasses the chosen policy.

## 10. Filesystem and terminal integration

- Local shell execution is sufficient for the initial local-agent implementation;
  client terminal hosting is optional, subject to the chosen protocol version.
- For v1, evaluate negotiated filesystem/terminal client methods separately. Do not
  promise support for unsaved editor buffers while reading only local disk.
- For v2, follow its tool/terminal representation rather than importing v1 methods.
- If client-hosted terminals are added, implement create/output/wait/kill/release
  semantics applicable to that version, including cleanup after disconnect.
- Preserve cwd, timeout, output bounds, cancellation, and permission behavior.
- Document initial limitations for unsaved buffers, remote workspaces, and Windows;
  the existing shell/session implementation contains Unix-specific operations.

Acceptance: the supported local-workspace path is documented and works end to end;
no unsupported filesystem/terminal capability is advertised.

## 11. Cancellation, backpressure, and shutdown

- Distinguish protocol request cancellation from session turn cancellation.
- Cancel provider HTTP requests, pending approvals, active shell process groups,
  and outstanding client RPCs belonging to the affected turn.
- Define whether detached background tasks survive a cancelled turn. Preserve the
  intended existing behavior and document it; connection shutdown must not leave
  unmanaged processes or locked sessions.
- Resolve races between cancellation and successful completion exactly once.
- Drain required terminal updates before the prompt response where possible.
- Bound shutdown waits; do not hang indefinitely on a client that stopped reading.
- Save recoverable state and release database/session/socket resources on EOF/signals.

Acceptance: cancellation works during streaming, approval, tools, and idle waits;
repeated cancellation and disconnect do not leak processes, locks, or goroutines.

## 12. Test strategy

Use a deterministic fake provider and fake ACP client; no paid model calls in CI.

Transport and contract:
- Valid/invalid JSON, malformed envelopes, unknown methods, invalid state/params.
- Version negotiation, capability matrix, missing credentials, secret redaction.
- Concurrent responses, bidirectional requests, fragmented reads, large frames,
  slow writers, EOF, and stdout containing only protocol frames.

Sessions and prompts:
- Text streaming without duplicates, tool cycles, empty/unsupported content.
- Provider errors with empty/nested fields, retries, incomplete responses.
- Session isolation, persisted restore/replay, locked sessions, storage failures.
- Session-scoped model settings and cwd; repeated overlapping prompt requests.

Safety and lifecycle:
- Approval/denial, cancelled approval, unresponsive client, no-capability policy.
- Cancellation at every phase, completion races, tool timeout, subprocess cleanup.
- Background tasks and helper callbacks cannot cross sessions or bypass permissions.
- Buffer overflow and transport failure resolve pending operations deterministically.

Regression and interoperability:
- Run `make test` including formatting, vet, and the race detector.
- Add executable-level stdio tests launching the actual binary with isolated home.
- Manually verify the pinned editor release: launch, text, tool approval, rejection,
  cancellation, provider failure, close/reopen, and all advertised restore features.
- Record exact editor/protocol versions and reproducible setup instructions.

## 13. Delivery milestones

### A. Contract and baseline
- Select editor/version/schema and SDK strategy.
- Resolve baseline formatting, source-budget decision, and empty diagnostics.
- Deliver compatibility matrix and architecture decisions.

### B. Safe protocol skeleton
- Add `dashy acp`, transport, initialization, session creation, text prompts,
  cancellation, clean shutdown, and isolated fake-peer tests.
- Disable shell execution until permission plumbing is ready.
- Exit gate: no stdout pollution, no hung prompts, no cross-session leakage.

### C. Useful coding-agent preview
- Add structured tool events, permission requests, local shell execution, provider
  failure handling, backpressure, and task/helper integration.
- Exit gate: real editor can complete an approved coding task and safely cancel it.
- Label as preview; document supported protocol version and limitations.

### D. Persistence and richer input
- Add version-appropriate load/resume and replay, images, and session-scoped model
  configuration where supported. Advertise each capability only after tests pass.
- Optional client filesystem/terminal integration is a separate scoped increment.

### E. Release hardening
- Complete process-level tests, race checks, editor smoke tests, security review,
  documentation, and source-budget verification.
- Add packaging/launch examples and version-specific compatibility notes.

No calendar estimate is committed before milestone A: SDK/version choice, source
budget, and permission integration materially affect the implementation size.

## Definition of done

- The chosen editor launches Dashy using `dashy acp` without TUI artifacts.
- Every advertised capability has contract tests and an interoperability check.
- Text and tool events are ordered, correlated, and not duplicated.
- Approvals gate execution; denial, cancellation, and disconnect are safe.
- Sessions remain isolated; persisted state can be restored if advertised.
- Credentials are redacted and unsupported features fail explicitly.
- Existing TUI, print, helper, and background-task behavior passes regression tests.
- `make test` passes and the agreed production-source budget is met.
