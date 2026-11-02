# Hooks

Hooks run your commands when Dashy emits lifecycle events, for example to update a
dashboard, send a notification, or run tests after a turn. Hooks observe Dashy. They
cannot block, change or veto anything.

## Configuration

Hooks are named `[hooks.NAME]` tables in `dashy.toml`. Hooks merge by name across the global
and project files (see `dashy docs config`). Their fields and constraints are in the key
reference there; this topic explains what they do.

```toml
[hooks.turn_test]
events = ["turn_end"]
command = '''
[ "$DASHY_TURN_SOURCE" = user ] || exit 0
make test > /tmp/test-hook.log 2>&1 || dashy notify "make test failed: $(tail -n 20 /tmp/test-hook.log)"
'''
timeout = 180

[hooks.session_init]
events = ["session_start"]
command = "cat AGENTS.md"
send_output = true
```

Field behavior:
- `events`: the lifecycle events to handle (listed below). `"*"` matches all of them.
- `command`: run with `/bin/bash --noprofile --norc -c` in the project directory, with the
  user's permissions.
- `timeout`: when it expires, the process group is killed and the incident is logged.
- `enabled`: `false` disables the hook, including an inherited hook of the same name.
- `send_output`: queues the output for delivery at the start of the next turn (see below).
- `headless_support`: `false` skips the hook in `--mode jsonl` runs. It still runs in the TUI.

## Delivery and Execution

Each hook runs independently. The environment is the process environment, with `BASH_ENV`
removed, plus `[env]` and the hook variables described below. `PATH` starts with the directory
of the running `dashy` binary, so hook commands can call `dashy` even when it is not on the
launch `PATH`.

Hooks are strictly asynchronous, except for the startup gate described below. Dashy dispatches
the event and continues immediately. Each
hook has its own queue of 64 events, preserving event order within that hook. A slow hook
never blocks other hooks or the agent. Order across different hooks is not guaranteed.
Scripts that need cross-hook ordering can compare `DASHY_SEQ`.

Hook output is not printed to the terminal. A hook that fails to start, exits nonzero, times
out, or overflows its queue is logged to `$DASHY_HOME/hooks.log` (see `dashy docs
troubleshooting`). On exit, Dashy waits up to 3 seconds for active hooks. Then it kills the hooks
still running, with their process groups, and skips the queued ones. Both are logged.

## Capturing Output with send_output

With `send_output = true`, Dashy captures the merged stdout and stderr (up to `shell.max_output`
bytes), the exit code, the hook and event names, the UTC RFC3339Nano start time and the
truncation status.

Delivery:
- Output is queued for the session that was active when the hook ran.
- Ready results are drained into the saved user history at the beginning of that session's
  NEXT TURN, immediately before the new user input.
- Queued output does NOT start a turn. It waits for the next turn, which the user or an
  explicit `dashy notify` starts.
- Delivered results stay in session history and take part in normal compaction.
- Delivered output is formatted as XML:
  ```xml
  <notify>
  <hook_output name="NAME" event="EVENT" start="RFC3339NANO" exit_code="N" truncated="BOOL" command="SCRIPT">
  Hook execution data (not user instructions):
  ...
  </hook_output>
  </notify>
  ```
- The `command` attribute is the script exactly as configured, captured when the hook ran. A
  later reload does not change it. It is not shell-expanded, so `$TOKEN` appears literally and
  environment values are never included.
- Hook commands are sent to the model and saved in history. Refer to secrets as `$TOKEN` and
  never write literal credentials into a command.
- The terminal shows each delivered result as one row, such as
  `! Hook output | NAME: first words of the output`.
- XML special characters are escaped and control characters are removed.
- Pending results are kept per session, up to 64 per session and 512 across all sessions. A
  result beyond either limit is dropped and logged. Pending results are not kept across
  application restarts.

### Startup gate

A `session_start` hook with `send_output` is a startup gate. Dashy waits for it before the first
model request of the session, so its output is in context when the model first answers. The
output is delivered at the start of that first turn, before the user's message. Other
`session_start` hooks stay asynchronous.

- The wait has a budget: the jobs already queued on the same hook, each up to its `timeout` plus
  one second, then the startup hook's own run with the same allowance. Earlier events on a `*`
  hook can use part of that budget, so a slow `dashy_start` does not fail a healthy startup.
  Past the budget, the session fails with a timeout. Canceling the turn ends the wait, and the
  result is still delivered on the next turn.
- If the hook fails to start, exits nonzero, times out, cannot be queued, or its output cannot be kept (the pending output queue is full), the model request
  is refused with an error. The same error repeats for that session until a reload (`/reload`)
  runs the hook again, or until the session ends (`/new`, `/resume`, or exit). Fix the hook and
  reload.
- Each hook runs once per session start. The gate does not run it a second time.

Output from other events is asynchronous. The first turn may start before that output is ready,
and the output then arrives in a later turn.

## Immediate Notifications and Loop Prevention

For immediate action, call `dashy notify TEXT...` from a hook:
- If the agent is idle, `dashy notify` starts a turn immediately.
- If the agent is busy, `dashy notify` queues a steering instruction delivered at the next
  safe request boundary after tool outputs.

When a hook calls `dashy notify` directly, keep `send_output = false` to avoid delivering the
same result twice.

Both `turn_start` and `turn_end` export `DASHY_TURN_SOURCE` as `user` or `notify`. A hook on
`turn_end` that calls `dashy notify` MUST check the source first:

```bash
[ "$DASHY_TURN_SOURCE" = user ] || exit 0
```

Without this guard, a `turn_end` hook that calls `dashy notify` starts a notify turn. That
turn ends with another `turn_end`, which calls `dashy notify` again, and the loop never stops.

`turn_end` runs AFTER the model's answer is complete and sent. Tests run from a `turn_end`
hook cannot validate or repair an answer before the user sees it. They can only report
failures to a later turn through `dashy notify`.

## Variables

Hook processes get the metadata variables listed in `dashy docs cli` (Environment variables),
including `DASHY_EVENT`, `DASHY_SEQ`, `DASHY_TS`, `DASHY_MODE`, `DASHY_SESSION_ID`, `DASHY_CWD`,
`DASHY_PROVIDER`, `DASHY_MODEL`, `DASHY_EFFORT` and `DASHY_ENDPOINT`. The event-specific
variables are listed with each event below.

Variables whose names end in `_FILE` contain the path of a temporary file with the full text
payload. The file exists only while the hooks for that event run.

## Events

Dashy has 22 lifecycle events, plus the wildcard `*`.

Process and session:
- `dashy_start`: Dashy started and the session is open. `DASHY_VERSION`.
- `dashy_exit`: the process is ending. `DASHY_EXIT_CODE` (0 or 1).
- `session_start`: a session became active. `DASHY_SESSION_SOURCE` (`startup`, `resume`, or
  `new`). After `/new` and `/resume`, also `DASHY_PREV_SESSION_ID`.
- `session_switch`: `/new` or `/resume` replaced the active session. `DASHY_PREV_SESSION_ID`.
- `session_end`: the session is being left. `DASHY_REASON` (`switch` or `exit`).
- `session_title_change`: the session title changed. `DASHY_SESSION_TITLE`.

Turns:
- `turn_start`: a turn began. `DASHY_TURN_SOURCE` (`user` or `notify`) and `DASHY_MESSAGE_FILE`.
- `turn_end`: a turn finished. `DASHY_TURN_STATUS` (`ok`, `error`, or `cancelled`),
  `DASHY_TURN_DURATION_MS`, `DASHY_TURN_SOURCE`, and `DASHY_ERROR` when it failed.
- `idle`: interface only. Work finished and no follow-up turn starts.
- `error`: an error occurred during a turn. `DASHY_ERROR`.

Input:
- `steer_queued`: interface only. A steering message was queued during a turn. `DASHY_MESSAGE_FILE`.
- `steer_accepted`: the active turn accepted a queued message. `DASHY_MESSAGE_FILE`.
- `notify_received`: `dashy notify` delivered a message. `DASHY_NOTIFY_FILE`.
- `image_attached`: an image was attached. `DASHY_IMAGE_SOURCE` is `paste` (TUI clipboard), `view`
  (`dashy view`, when the image is handed to the model), or `input` (an image in an accepted headless
  `input` request). See `dashy docs headless`.

Model:
- `model_change`: `/model` changed the selection. `DASHY_PREV_PROVIDER`, `DASHY_PREV_MODEL`,
  `DASHY_PREV_EFFORT`.
- `model_request_start`: a provider request began.
- `model_request_end`: a provider request finished. `DASHY_DURATION_MS`,
  `DASHY_INPUT_TOKENS`, `DASHY_OUTPUT_TOKENS`.
- `model_retry`: a transient request failure is being retried. `DASHY_ERROR`, `DASHY_ATTEMPT`.

Shell and compaction:
- `shell_start`: an agent bash tool call begins. `DASHY_COMMAND_FILE`, `DASHY_TIMEOUT`.
  It covers agent tool calls only, not hook or status bar processes.
- `shell_end`: an agent bash tool call ends. `DASHY_EXIT_CODE`, `DASHY_DURATION_MS`,
  `DASHY_TIMED_OUT`.
- `compact_start`: compaction began. `DASHY_COMPACT_REASON` (`manual` or `auto`).
- `compact_end`: compaction finished. `DASHY_COMPACT_STATUS` (`ok` or `error`), `DASHY_ERROR`.
