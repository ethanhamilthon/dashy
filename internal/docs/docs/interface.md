# Interface

## Slash Commands

- `/new`: starts a new chat and session. Configuration files are re-read.
- `/resume`: selects a project session to reopen (see `dashy docs sessions`).
- `/rename [name]`: renames the session. An empty name restores the default title, the first user message.
  Refused while a turn runs.
- `/compact [prompt]`: summarizes older turns (see `dashy docs sessions`).
- `/reload`: re-reads configuration for this chat. History and the active provider, model and
  effort are kept. Running shell commands are stopped. Refused while a turn runs.
- `/model`: selects the active model and reasoning effort (see `dashy docs models`).
- `/login`: selects an existing provider or adds a new one (see `dashy docs models`).
- `/help`: lists the commands and shortcuts.
- `/quit`: saves the history and exits.

Up and Down move through completions, Tab completes, and Enter runs the selected visible slash
command at once. Without a matching visible completion, text that starts with a slash is an ordinary
prompt. Esc dismisses an open completion menu.

## File Path Completion (@)

Typing `@` in the composer opens path completion at the cursor:
- Search runs asynchronously inside the launch project root. It uses Git's tracked and unignored
  untracked files (`git ls-files`), with a bounded directory walk as fallback. `.git`, `.dashy` and
  `node_modules` are skipped, and directory symlinks are not followed.
- Email addresses and URLs such as `https://.../@name` are not treated as references.
- The menu shows at most 8 candidates. Up and Down move through them. Tab or Enter inserts the
  selected relative path.
- Accepting replaces only the active `@` token, keeps the rest of the text, and keeps the cursor
  position. Paths with spaces or quotes are quoted automatically, for example `@"path with spaces/file.txt"`.
- Accepting a directory appends `/` and keeps the menu open.
- Accepting a file does NOT submit anything. Only a later Enter submits the message.
- Completion works with drafts, attached images and busy turns. Enter accepts the path first, without
  clearing images or sending a steering message early.
- Esc closes an active or scanning menu before it cancels an in-flight turn.

## @ File References

A submitted prompt attaches the content of each `@path` it names:
- Each distinct path is read once, in first-mention order, relative to the launch project root.
  A prompt may attach at most 32 distinct files. More report an error and keep the draft.
- Only regular UTF-8 text files attach. Directories are not expanded. Files that contain NUL bytes or
  are not valid UTF-8, special files, and paths or symlinks that resolve outside the project root
  report an error. Images use their own flow.
- Reading happens when Enter is pressed, not when the path is selected. The draft stays in the
  composer until the files are read. Esc cancels the read and keeps the draft. If the draft changes
  during the read, the edits are kept and nothing is sent; press Enter again.
- A missing or unreadable file reports an error and keeps the draft, so the model never receives a
  prompt without the content you asked for.
- Each file is limited by `max_file_bytes` (see `dashy docs config`). Longer files send their leading
  bytes, marked as truncated with the included and original sizes. The limit applies per file, so
  several large files can still exceed the model's context window.
- Emails, URLs, escaped `\@` and a bare `@` attach nothing.
- The composer and transcript show only the prompt. The model gets the prompt and the file contents in
  one user message. File contents are labelled as data with their boundaries escaped. They are not
  filtered for secrets or instructions.
- A queued steering instruction keeps the snapshot taken when it was submitted, including when it is
  promoted to a follow-up turn. Recalling it restores editable text, and resubmitting reads the files again.
- Notifications and headless prompts are not scanned for `@` references. Headless clients pass
  `files` explicitly (see `dashy docs headless`).

## Padded Status Bar

The bottom status bar has exactly 2 display cells of padding on each side. Content width is
`max(0, width - 4)`. Narrow terminals (1 to 5 cells) are handled without wrapping or negative padding.
Wide Unicode characters and emoji are truncated by display width.

Segments come from `ui.status_left` and `ui.status_right` (keys in `dashy docs config`):
- Each command runs in `/bin/bash --noprofile --norc -c` in the project directory, with inherited
  `BASH_ENV` removed.
- The environment is the process environment, `[env]`, and the status metadata variables in
  `dashy docs cli`. There are no `DASHY_*_LABEL` variables and no formatting helpers. Commands build
  their own labels and handle missing values.
- Non-empty stdout is trimmed, flattened, stripped of ANSI and OSC sequences, and joined with
  `" · "` in declared order. Empty or failing commands are omitted. Failures and timeouts are logged to
  `status.log` (see `dashy docs troubleshooting`).
- Execution is bounded: 500 ms per command, 1 KiB of output per command, and at most 4 commands at a time.
- Evaluation runs on every lifecycle event (all 22), on the first render, and after a successful `/reload`.
  There is no polling and no timer. A new event cancels in-flight commands and starts a new evaluation.
- Status commands do not emit lifecycle events, so they cannot refresh themselves.
- No status command runs in headless mode.

## Editing and Navigation

- Alt+Enter or Ctrl+J inserts a newline. Shift+Enter works on compatible terminals.
- PgUp and PgDn scroll the timeline. Ctrl+Home and Ctrl+End jump to its start and end.
- Ctrl+C cancels active work, or exits when idle.
- Up on an empty composer recalls the newest unaccepted queued steering instruction.
- Dragging the left mouse button over the timeline selects text and copies it to the system clipboard
  on release. The mouse wheel scrolls.
- Visible body rows for code, results and reasoning are set under `[ui]` (see `dashy docs config`).
- `dashy docs clipboard` covers image paste (Ctrl+V). `dashy docs headless` covers the JSON transport,
  which runs the same operations as this interface.
