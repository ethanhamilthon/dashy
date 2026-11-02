# dashy

dashy is a minimal AI coding agent with an interactive terminal interface (TUI) and a one-shot execution mode (`-p`).

In TUI mode, dashy emits OSC 7501 program status reports (idle, working, done, error) to supported terminals.

## Installation

Install the latest binary:

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/dashy/main/scripts/install.sh | sh
```

The binary is installed to `~/.local/bin/dashy` by default (customize with `DASHY_BINDIR`; pin a release with `DASHY_VERSION`).

Or build from source (Go 1.26+):

```sh
git clone https://github.com/ethanhamilthon/dashy && cd dashy
make install
```

## System Prompt (AGENTS.md) and dashy Blocks

The system prompt is assembled lazily once per session from `AGENTS.md` files:
1. Global instructions from `$DASHY_HOME/AGENTS.md` (default `~/.dashy/AGENTS.md`; skipped with `--no-global`).
2. Local instructions: `./AGENTS.md` or the file specified by `--config FILE`.

Fenced code blocks with info string `dashy` (```` ```dashy ````) run via `bash -c` in the project directory when the prompt is assembled. Their output replaces the block in the system prompt. Blocks time out after 30 seconds and are capped at 64 KiB of output.

## Sub-Agents (`dashy -p`)

Run tasks in one-shot mode with `dashy -p "TASK"`. The final assistant response is printed to stdout and progress to stderr:

```sh
# Run a focused task with a dedicated prompt and no global instructions
dashy -p "Review diff against main" --config .dashy/reviewer.md --no-global

# Run parallel sub-agents from bash
dashy -p "Task A" > a.out 2> a.err &
dashy -p "Task B" > b.out 2> b.err &
wait

# Background command that notifies the running session when done
(make test > test.log 2>&1; dashy notify "Tests completed with status $?") &
```

## Settings (`/settings`)

All configuration is stored in `~/.dashy/settings.json`. Manage providers, models, and preferences inside the app:
- Run `/settings` to choose an active provider and model, adjust reasoning effort (Left/Right), toggle reasoning summaries and auto-compaction, or cycle visible output lines (short/medium/full).
- Run `/login` (or select "Add provider" in `/settings`) to configure a provider name, HTTP(S) base URL, and API key.

Run `dashy --help` for the complete command and flag reference.
