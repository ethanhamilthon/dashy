# dashy

dashy is a minimal AI coding agent with an interactive terminal UI (TUI) and a one-shot mode (`dashy -p`). The model gets one tool, `bash`; its instructions come from AGENTS.md files.

In the TUI, dashy reports its state (idle, working, done, error) to terminals that support OSC 7501.

## Install

From a checkout of this repository (Go 1.26+):

```sh
make install      # builds dashy into ~/.local/bin (set BINDIR=... to change it)
dashy version
```

The release one-liner below does not work yet, because no public release exists at that address:

```sh
curl -fsSL https://raw.githubusercontent.com/ethanhamilthon/dashy/main/scripts/install.sh | sh
```

## First start

Run `dashy` in a project directory. Open `/settings` to choose a provider and model (`/login` adds a provider). Settings are saved to `$DASHY_HOME/settings.json` (default `~/.dashy/settings.json`).

`dashy models` prints the exact model IDs of every configured provider, one `PROVIDER<TAB>MODEL` per line (`--provider NAME` limits it to one provider).

## AGENTS.md and dashy blocks

The system prompt is built from two AGENTS.md files before the first request of a session (`/new`, `/resume` and `/reload` build it again):

1. `$DASHY_HOME/AGENTS.md` (default `~/.dashy/AGENTS.md`), skipped with `--no-global`.
2. `./AGENTS.md`, or the file given with `--config FILE` (which replaces `./AGENTS.md`).

A fenced block with the info string `dashy` runs with `bash -c` in the project directory, and its output replaces the block. Blocks time out after 30 seconds and are capped at 64 KiB of output.

````markdown
# Project notes

The secret word is PINEAPPLE.

```dashy
git log --oneline -3
```
````

## Sub-agents (`dashy -p`)

`dashy -p "TASK"` runs one task in a new session (or in a saved one with `--resume ID`): the final answer goes to stdout, progress to stderr, and the exit code is non-zero on failure. A sub-agent has its own `dashy notify` and `dashy view` while it works; to report to another session, pass `-s` with that session's ID.

```sh
# A focused reviewer with its own instructions and no global ones
dashy -p "Review the diff against main" --config .dashy/reviewer.md --no-global

# Another provider, model or reasoning effort for this run only (settings stay unchanged)
dashy -p "Summarize the diff" --provider openai --model gpt-5-mini --effort low

# Parallel sub-agents
dashy -p "Task A" > a.out 2> a.err &
dashy -p "Task B" > b.out 2> b.err &
wait

# Background work that tells the running session when it is done
(make test > test.log 2>&1; dashy notify "tests finished with status $?") &
```

## Development

`make test` runs the same checks as CI: gofmt, `go vet` and the tests with the race detector.

Run `dashy --help` for the full reference.
