# Troubleshooting

Startup and configuration errors stop Dashy before the interface opens. The message is printed on
stderr and the exit status is 1. File errors start with the path of the file that failed. Messages
that appear during a turn are shown in the timeline or in the JSON `error` record.

## Startup and Configuration Errors

| Message (contains) | Cause | Fix |
| --- | --- | --- |
| `config PATH: stat PATH: no such file or directory` | `--config` or `-c` names a missing file. | Check the path. A relative path resolves against the launch directory. |
| `config PATH: is a directory` | `--config` names a directory. | Pass the file itself. |
| `missing field` (after the file path, with the offending line shown) | Unknown key or table, usually a typo. Strict mode rejects it. | Correct the name. The valid keys are in `dashy docs config`. |
| `toml: ...` after the file path | Invalid TOML syntax. | Fix the syntax at the line the message points to. |
| `hook "NAME" needs events and command` | An enabled hook lacks `events` or a non-blank `command`. | Add both, or set `enabled = false`. |
| `hook "NAME": unknown event "EVENT"; see dashy docs hooks` | Event name is not one of the 22 events or `"*"`. | Use a name from `dashy docs hooks`. |
| `hook "NAME": timeout must be 1..600 seconds` | Hook `timeout` is negative or above 600. | Use 1 to 600, or omit it for 10. |
| `selected provider "NAME" is not configured` | `provider` names a provider with no `[providers.NAME]` table. | Add the table with `/login` or by hand, or change `provider`. |
| `provider "NAME" has conflicting api_key and api_key_env` | Both are nonempty after merging. | Keep one. Set the other to `""` in the file that sets it. |
| `provider "NAME": environment variable "VAR" is not set or empty` | The selected provider's `api_key_env` names an unset or blank variable. | Export `VAR` before starting Dashy, or add it under `[env]`. |
| `provider "NAME" needs an HTTP(S) base_url` | `base_url` is not an HTTP or HTTPS URL with a host. | Use a full URL such as `https://host/v1`. |
| `env key "DASHY_..." is reserved` | `[env]` sets a `DASHY_` variable. | Rename the key. The `DASHY_` prefix belongs to Dashy. |
| `reasoning_summary must be auto or off` | Other value. | Use `auto` or `off`. |
| `unsupported effort "X"` | `effort` is not in the allowed list. | Use one of the values in `dashy docs config`. |
| `max_*_lines must be non-negative` | A `[ui]` row limit is negative. | Use 0 or more. |
| `max_context_tokens, compact_keep_turns, max_agent_steps and max_file_bytes must be non-negative` | One of these is negative. | Use 0 or more. |
| `shell timeout and max_timeout must be positive` | `shell.timeout` or `shell.max_timeout` is 0 or less. | Use values above 0. |
| `shell timeout (N) cannot exceed max_timeout (M)` | Default timeout is above the maximum. | Lower `shell.timeout` or raise `shell.max_timeout`. |
| `shell max_output must be non-negative` | `shell.max_output` is negative. | Use 0 (no truncation) or more. |

Provider, shell and hook checks run on the merged configuration. Their message names the primary file,
even when the bad value came from the global file, so check both files. See `dashy docs config`.

## Runtime Errors

| Message (contains) | Cause | Fix |
| --- | --- | --- |
| `no provider or model selected` | Provider or model is empty when a request starts. | Run `/login`, then `/model`. |
| `session is in use by another dashy process: ID` | Another process owns that session (for example a headless run). | Close the other process, or use a different session. |
| `session not found in this project: ID` | `-resume` or `/resume` names an unknown session. | Run `/resume` to list the project's sessions. |
| `invalid session ID` | An ID contains a path separator. | Pass the ID exactly as listed. |
| `stopped after N agent steps; raise max_agent_steps in dashy.toml` | The per-turn tool limit was reached. | Raise `max_agent_steps` or set it to `0`. |
| `timeout must be 1..N seconds` | A bash call asked for a timeout outside the limit (the result has exit code 2). | Use a value between 1 and `shell.max_timeout`. |
| `max_output must be >= 0 bytes` | A bash call passed a negative `max_output`. | Use 0 or more. |
| `outside a dashy shell, pass -s SESSION_ID` | `dashy notify` or `dashy view` ran without `DASHY_ENDPOINT`. | Pass `-s SESSION_ID`. |
| `session is inactive: ID` | The target session is not running. | Start the session first. |
| `notify requires 1..131072 bytes of message text` | Empty or over 128 KiB. | Send less text. |
| `notification queue is full` | 64 notifications are waiting. | Retry after the agent takes the queue. |
| `view queue is full` | Four images wait for the next request. | Retry after the next request. |
| `view requires a valid PNG or JPEG image`, `view image exceeds 32 MiB`, `view image exceeds 20 megapixels` | Input rejected by the image checks. | See the limits in `dashy docs images`. |
| `at most 32 files per message` | More than 32 distinct `@` files. | Split the prompt. See `dashy docs interface`. |
| `@PATH: outside the project root`, `@PATH: not a regular file`, `not valid UTF-8 text`, `contains NUL bytes; not text` | A `@` reference cannot be attached. The draft is kept. | Use a text file inside the project. |
| `At most eight images may be pending` | Pending image limit reached (`attachment_limit`). | Submit or remove images. See `dashy docs clipboard`. |
| `Steering queue is full` (`queue_full`) | 64 steering instructions are waiting. | Wait for the active turn to accept some. |
| `Wait for the active turn before reloading settings.` (`busy`) | `/reload` during a turn. | Wait, or cancel with Ctrl+C. |
| `could not save history` | The session file could not be written. | Check disk space and the permissions of `$DASHY_HOME/sessions`. |

## Retries

A transient provider failure is retried, and the timeline shows
`Retrying in Ns (attempt N/3): ...`. See `dashy docs models` for the retry rules.

## Log Files

Both logs live in `$DASHY_HOME` (default `~/.dashy`) and are created with mode 0600.

- `hooks.log`: one line per hook incident: a hook that fails to start, exits nonzero, times out,
  overflows its queue, or whose output result is dropped. Format:
  `<RFC3339 time> event=EVENT command="SCRIPT": PROBLEM`.
- `status.log`: one line per failed or timed-out status command, at most one line per second, with
  the message cut to 200 bytes. Format: `<RFC3339 time> cmd="COMMAND": MESSAGE`.

Rotation: before a line is appended, a log of 1 MiB or more is renamed to `NAME.1`, replacing the
previous `.1`. Only one previous generation is kept. Line breaks inside a message become spaces.
