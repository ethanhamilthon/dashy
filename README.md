# dashy

dashy is a minimal AI coding agent with an interactive terminal UI (TUI) and local ACP editor integration (`dashy acp`). The model gets one tool, `bash`; its instructions come from DASHY.md files.

## Install

From a checkout of this repository (Go 1.27.3+):

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

Settings apply on the fly: changes made in the TUI take effect immediately, even while the agent works, and changes saved by another dashy process are picked up before the next model request. The running turn is not interrupted; its next request uses the new values. Each open session keeps its own provider/model selection.

`dashy models` prints the exact model IDs of every configured provider, one `PROVIDER<TAB>MODEL` per line (`--provider NAME` limits it to one provider).

## DASHY.md and dashy blocks

The system prompt uses a single instruction file before the first request of a new session. Both TUI and ACP select the first applicable option:

1. `--config FILE`: use only this file. A missing file is an error.
2. `./DASHY.md` in the current directory, if present.
3. `$DASHY_HOME/DASHY.md` (default `~/.dashy/DASHY.md`) as a fallback.

Files are never merged, and parent directories are not searched. If no file exists, instructions are empty. `/resume` reuses its stored snapshot; `/reload` selects the file again and replaces the snapshot.

A fenced block with the info string `dashy` runs with `bash -c` in the project directory, and its output replaces the block. Blocks time out after 30 seconds and are capped at 64 KiB of output. Opening an untrusted project's `DASHY.md` can execute arbitrary code with your permissions: review its blocks before starting Dashy.

The assembled prompt is saved with the session. `/resume` reuses that snapshot; `/reload` refreshes it from the current instruction files and blocks.

````markdown
# Project notes

The secret word is PINEAPPLE.

```dashy
git log --oneline -3
```
````

## Editor integration (ACP preview)

Launch `dashy acp` from an ACP v1 editor. Configure a provider/model using the TUI
first. Shell commands, instruction blocks and task helpers execute freely, just as
in the TUI: there are no permission prompts or sandbox. Use trusted projects only.

See [ACP compatibility, launch configuration and limitations](docs/ACP.md).

## Development

`make test` runs the same checks as CI: gofmt, `go vet` and the tests with the race detector.

Run `dashy --help` for the full reference.
