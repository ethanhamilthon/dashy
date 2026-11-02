# Command line reference

This topic owns every command, flag, environment variable and exit code.

## Commands

- `dashy`: start the interactive TUI in the current directory (the project root).
- `dashy --mode jsonl`: run without the TUI, JSONL over stdin and stdout. This is the only
  non-interactive mode (see `dashy docs headless`).
- `dashy notify [-s SESSION_ID] [--] TEXT...`: send text to a running session. Reads stdin when
  no text is given. Inside a dashy shell the session comes from `DASHY_ENDPOINT`. Elsewhere `-s` is required.
- `dashy view [-s SESSION_ID] PATH...`: queue PNG or JPEG images for the next model request
  (see `dashy docs images`).
- `dashy docs [TOPIC]`: print built-in documentation. `dashy docs index` lists the topics.
- `dashy update [--check]`: install the latest release, or only report whether one exists
  (see `dashy docs update`).
- `dashy version`: print the installed version.

Go-style flags accept one or two dashes: `-config` and `--config` are the same.

## Flags

- `-mode jsonl`: run the headless JSONL transport instead of the TUI.
- `-resume SESSION_ID`: reopen an existing project session, in the TUI or headless. An unknown ID is an error.
- `-config PATH`, `-c PATH`: use this file as the primary configuration (precedence in `dashy docs config`).
- `-agent PATH`: overlay a role configuration file, applied last over the primary file. The file must exist (precedence in `dashy docs config`).
- `-no-extend`: do not inherit the global `$DASHY_HOME/dashy.toml`.
- `-s SESSION_ID`: target session for `notify` and `view`.

Positional arguments are an error except for `notify` and `view`. The removed `-prompt` flag
is rejected. `-mode` accepts only `jsonl`.

## Environment variables

One table covers every `DASHY_*` variable. Columns say where a variable is available:
- Shell: the agent's bash tool calls (the process inherits the rest of the launch environment).
- Hooks: hook processes (`dashy docs hooks`).
- Status: `status_left` and `status_right` commands (`dashy docs interface`).

Variables you set (inherited means the value comes from the launch environment, so it reaches all three):

| Variable | Meaning | Shell | Hooks | Status |
| --- | --- | --- | --- | --- |
| `DASHY_HOME` | Data and global configuration directory. Default `~/.dashy`; a relative value resolves against the launch directory. Read at startup. | inherited | inherited | inherited |
| `DASHY_NO_UPDATE_CHECK` | Any nonempty value disables the startup update check (see `dashy docs update`). | inherited | inherited | inherited |

Variables dashy sets for every call or event:

| Variable | Meaning | Shell | Hooks | Status |
| --- | --- | --- | --- | --- |
| `DASHY_ENDPOINT` | Session socket used by `dashy notify` and `dashy view`. Do not set it by hand except to target an outer session (see `dashy docs cookbook`). | yes | yes | lifecycle events only |
| `DASHY_SESSION_ID` | ID of the active session. | yes | yes | yes |
| `DASHY_PROVIDER` | Active provider name. | yes | yes | yes |
| `DASHY_MODEL` | Active model name. | yes | yes | yes |
| `DASHY_EFFORT` | Active reasoning effort. Empty when unset. | yes | yes | yes |
| `DASHY_CTX_SIZE` | Latest request input tokens. Empty if unknown. | yes | no | yes |
| `DASHY_CWD` | Project directory. The shell starts there already, so the variable is not exported to it. | no | yes | yes |
| `DASHY_VERSION` | Dashy version. Hooks get it on `dashy_start`; the status bar gets it on every event. | no | dashy_start only | yes |
| `DASHY_STATE` | `idle`, `busy` or `cancelling`. | no | no | yes |
| `DASHY_MODE` | `tui` or `headless`. | no | yes | no |
| `DASHY_EVENT` | Name of the lifecycle event. The status bar's first render gets `init` and a `/reload` gets `reload`. | no | yes | yes |
| `DASHY_SEQ` | Monotonic event sequence number. | no | yes | no |
| `DASHY_TS` | Unix timestamp in milliseconds. | no | yes | no |

Event variables (`DASHY_SESSION_SOURCE`, `DASHY_PREV_SESSION_ID`, `DASHY_SESSION_TITLE`,
`DASHY_REASON`, `DASHY_EXIT_CODE`, `DASHY_TURN_SOURCE`, `DASHY_TURN_STATUS`,
`DASHY_TURN_DURATION_MS`, `DASHY_ERROR`, `DASHY_PREV_PROVIDER`, `DASHY_PREV_MODEL`,
`DASHY_PREV_EFFORT`, `DASHY_DURATION_MS`, `DASHY_INPUT_TOKENS`, `DASHY_OUTPUT_TOKENS`,
`DASHY_ATTEMPT`, `DASHY_TIMEOUT`, `DASHY_TIMED_OUT`, `DASHY_COMPACT_REASON`,
`DASHY_COMPACT_STATUS`, `DASHY_IMAGE_SOURCE`, and the payload paths `DASHY_MESSAGE_FILE`,
`DASHY_NOTIFY_FILE`, `DASHY_COMMAND_FILE`) are set only on the events that carry them.
Hooks receive all of them. The status bar receives the ones that are not `*_FILE` and not
`DASHY_TIMED_OUT`. The shell receives none. Meanings are listed per event in `dashy docs hooks`.

Configured `[env]` values are merged into the shell, hook and status environments. The `DASHY_`
prefix is reserved for dashy (see `dashy docs config`).

## Exit codes

- `dashy` and its subcommands: `0` on success; `1` on any error, with the message on stderr.
  A failed headless turn is reported in its `done` record and does not change the exit code.
- Bash tool results: `124` is a timeout, `130` is a cancellation, `127` means the shell could
  not start, and `2` is a syntax error or invalid tool arguments. Otherwise the command's own status is returned.
