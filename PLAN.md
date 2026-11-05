# Plan

Source budget at planning time: 8,871 production Go lines against the 7,000 limit in AGENTS.md.

## Implementation status

- Sections 1–5 implemented with regression tests.
- Audit fixes implemented: bounded streams, Go 1.27.3, atomic writes, controller
  initialization protected against callbacks, dead-code cleanup, macOS CI,
  release actions pinned to verified SHAs, and the untrusted-block warning.
- `make test` passes, including race detection. The controller initialization
  stress test also passes ten consecutive runs under `-race`.
- `govulncheck` reports no reachable vulnerabilities; two imported-package
  advisories remain outside the code's call paths.
- The source-budget policy was subsequently raised to 10,000 lines by agreement;
  AGENTS.md and CI enforce the new cap. The previous 7,000-line refactor is no
  longer required. ACP work and scope updates are tracked in ACP_PLAN.md.

The sections below retain the original specification and describe the old behavior.

## 1. Answer footer on one line

Now an answer has two metadata lines: tokens and cache in the header
(`assistant ↑28.6k ↓993 · cache 70%`), time and turn duration in the footer
(`19:08 · 45.3s`). Move everything into the footer:

    19:08 · 45.3s · ↑28.6k ↓993 · cache 70%

The header keeps only the label. Code labels keep their own `↑ ↓ · duration · cache`.

## 2. System prompt snapshot in the history

Now the prompt is assembled in memory before the first request and is never stored.
`/resume` assembles it again from the current DASHY.md files and dashy blocks, so a
resumed session can get a different prompt (and lose the prompt cache).

- Store the assembled prompt as a history item (own type, e.g. `{"type":"instructions",...}`),
  written before the first request of the session.
- `/resume` uses the latest stored snapshot and does not reassemble.
- `/reload` reassembles and appends a new snapshot; the latest one is sent.
- Snapshot items are never sent as input and are not shown in the timeline.
- Sessions without a snapshot (old ones) assemble once on resume and store it.

## 3. Per-item usage and time metadata

How it works now (`items.meta`, JSON):

- Only assistant message items get metadata. Function calls, tool outputs, reasoning
  and user messages store none.
- An answer stores the sum over all requests of the turn so far, not its own request.
  A turn with two answers stores cumulative numbers in both, so summing them double counts.
- Input is summed over requests: each request resends the context, so the turn input
  counts the shared prefix several times (billed tokens, not context size).
- A turn that ends without an answer (only tool calls, interrupted) stores no usage at all.
- Tool run durations are only shown live; after resume code shows `↑? ↓?` and
  outputs `?`.
- Metadata is looked up by the item's JSON text (`itemMeta`), not by its position.
  Works while provider item ids are unique, fragile otherwise.
- `RequestID` (`r-<history length>`) is stored but means nothing after resume.
- User messages have no time.

Target:

- Store metadata by `seq`, not by JSON text.
- Each response's items get that request's own usage (input, output, cached) and
  duration; the answer footer shows the turn total computed from them.
- Tool outputs store their run duration; user messages store the time they were sent.
- Keep `Cached = -1` for "provider did not report".
- Drop `RequestID` from the stored JSON.
- Tests: resume shows the same numbers as live; two answers in one turn; turn
  without an answer; provider without cached tokens.

## 4. Composer: keep click-to-place, drop selection

Keep: clicking in the composer moves the cursor there (needs the wrap layout and the
scroll recovery in `composer_selection.go`).

Remove: drag selection, its highlight, typing/backspace/paste over a selection,
Ctrl/Cmd+C and Ctrl/Cmd+X on a composer selection (`composerShortcut`).
Timeline selection and copy stay.

## 5. One file discovery

`internal/cli/files.go` and `internal/tui/files.go` both list files via
`git ls-files` with a filesystem fallback, with different rules (hidden files,
limits, cancellation). Share one small cancellation-aware lister with options;
completion and grep keep their own matching. Align `dashy grep` help with the
real hidden-file rule.

## 6. Leftovers from the audit

- Stream limits: one SSE token can reach 256 MiB, no event or response budget
  (`internal/agent/client.go`).
- Go toolchain: govulncheck flagged nine standard library advisories on 1.27.1;
  upgrade to a patched toolchain and rescan.
- File writes by `dashy write/patch` are not crash-atomic (temp file + rename).
- Controller registers its callback before it finishes initialization
  (`internal/control/controller.go`); needs a race stress test.
- Small dead code: `mdStable.src`, unused `readStdin` limit, unused `step`
  counter, unused `tinyPNG` test helper.
- CI: add the line-budget check, a macOS job, pin release actions to SHAs.
- Running dashy blocks from an untrusted project's DASHY.md executes its code;
  say so in the README.
