# Shell runtime

Long-form reference for the bash tool. The tool description sent to the model is the short form.

## Process Model

- Each call is a fresh `/bin/bash --noprofile --norc -c` process in the project directory.
  Nothing carries over between calls or turns: variables, functions, aliases and directory changes are lost.
- Put related steps in one command or in a script file.
- External programs are found through PATH and run with the user's permissions, each in its own process group.
  A script runs through the interpreter named by its shebang.
- The directory of the running dashy binary is first in PATH, so `dashy notify`, `dashy view`
  and `dashy docs` work inside scripts.
- There is no TTY and stdin is empty. PAGER and GIT_PAGER are set to `cat`. Shell startup files
  (`~/.bashrc`, `~/.zshrc`) are not read, and an inherited `BASH_ENV` is removed.
- The environment is the launch environment plus `[env]` and the shell metadata listed in
  `dashy docs cli`. Dashy does not change its own environment.

## Arguments and Results

- Each call returns `{output, exit_code}`. Stdout and stderr are merged.
- `timeout` (seconds) defaults to `[shell] timeout`. The argument must be between 1 and
  `[shell] max_timeout`; otherwise the call returns an error result with exit code 2.
- `max_output` (bytes) defaults to `[shell] max_output`. Output above the limit keeps its first
  and last halves, with a truncation notice in the middle. `0` keeps everything.
- A syntax error runs nothing and returns exit code 2. Exit codes are listed in `dashy docs cli`.
- Limits and defaults are in the key reference of `dashy docs config`.

## Timeouts and Cancellation

- On timeout the output gets a `[dashy: timed out after Ns]` line, the exit code is 124, and
  the process group is killed.
- Cancellation (Ctrl+C or Esc in the TUI, `stop` in JSON) ends the call with exit code 130.
  Shutdown and cancellation also kill the process groups of active calls.
- A child that keeps the output pipe open after the shell exits is cut off 250 ms later.

## Background Work

Work that outlives its call must redirect stdout and stderr to files and report back through
`dashy notify`:

```bash
(make release > /tmp/release.log 2>&1; dashy notify "release: exit $?") &
```

Background processes are not session jobs. Dashy tracks only the process groups of active
calls, so once the call returns it does not track or stop a background process.
