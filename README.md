# dashy

dashy is a minimal AI coding agent with an interactive terminal UI (TUI) and a one-shot mode (`dashy -p`). The model gets one tool, `bash`; its instructions come from DASHY.md files.

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

Run `dashy` in a project directory. The first run asks for a provider (any OpenAI-compatible endpoint) and a model. Use `/connect` to add, edit, delete or switch providers, `/models` to choose a model and reasoning effort, `/settings` for display and bash output options and `/tasks` to see or stop background tasks. A message sent while dashy works reaches the model right after the running command, which moves to the background as a task instead of making you wait. Settings and sessions are stored in the SQLite database `$DASHY_HOME/dashy.db` (default `~/.dashy/dashy.db`).

Settings apply on the fly: changes made in the TUI take effect immediately, even while the agent works, and changes saved by another dashy process are picked up before the next model request. The running turn is not interrupted; its next request uses the new values. `--provider`, `--model` and `--effort` keep priority over the stored settings.

`dashy models` prints the exact model IDs of every configured provider, one `PROVIDER<TAB>MODEL` per line (`--provider NAME` limits it to one provider).

## DASHY.md and dashy blocks

The system prompt is built from two DASHY.md files before the first request of a session (`/new`, `/resume` and `/reload` build it again):

1. `$DASHY_HOME/DASHY.md` (default `~/.dashy/DASHY.md`). The TUI loads it unless `--no-global`; `dashy -p` skips it unless `--global`.
2. `./DASHY.md`, or the file given with `--config FILE` (which replaces `./DASHY.md`).

A fenced block with the info string `dashy` runs with `bash -c` in the project directory, and its output replaces the block. Blocks time out after 30 seconds and are capped at 64 KiB of output.

````markdown
# Project notes

The secret word is PINEAPPLE.

```dashy
git log --oneline -3
```
````

## Sub-agents (`dashy -p`)

`dashy -p "TASK"` runs one task in a new session (or in a saved one with `--resume ID`): the final answer goes to stdout, progress to stderr, and the exit code is non-zero on failure. A sub-agent has its own `dashy tasks` and `dashy view` while it works and waits for its running tasks before it exits; to report to another session, pass `-s` with that session's ID.

```sh
# A focused reviewer with its own instructions (-p skips the global DASHY.md)
dashy -p "Review the diff against main" --config .dashy/reviewer.md

# A sub-agent that also follows your global DASHY.md
dashy -p "Refactor the parser" --global

# Another provider, model or reasoning effort for this run only (settings stay unchanged)
dashy -p "Summarize the diff" --provider openai --model gpt-5-mini --effort low

# Parallel sub-agents
dashy -p "Task A" > a.out 2> a.err &
dashy -p "Task B" > b.out 2> b.err &
wait

# Background task: the session gets a <task-result> when it ends
dashy tasks run make test
dashy tasks list
dashy tasks status t1
dashy tasks stop t1
```

## Development

`make test` runs the same checks as CI: gofmt, `go vet` and the tests with the race detector.

Run `dashy --help` for the full reference.
