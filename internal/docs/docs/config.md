# Configuration

Dashy is configured only by `dashy.toml` files. A file is read from up to two places and
merged over built-in defaults. This topic owns everything about those files; other topics
point here for discovery, precedence, merge rules and keys.

## Discovery and Precedence

The primary file is the first of these that applies:
1. `--config PATH` (or `-c PATH`). A relative path resolves against the launch directory.
   A missing, unreadable or invalid file is a startup error; nothing falls back.
2. `./dashy.toml` in the launch directory (the project root). There is no upward search.
3. `$DASHY_HOME/dashy.toml`, the global file. `DASHY_HOME` defaults to `~/.dashy`, so the
   default path is `~/.dashy/dashy.toml` (see `dashy docs cli`).

If no file exists, the built-in defaults apply.

`--agent PATH` adds a role overlay on top of the primary file. It does not replace the
primary file. The path must exist; a relative path resolves against the launch directory.

## Merge Order

Layers apply from lowest to highest: built-in defaults, then the global file
`$DASHY_HOME/dashy.toml`, then the primary file. If the primary and the global file are the
same physical file (a same-file alias), it is read once.

Layers apply in this order: built-in defaults, the global file, the primary file, then the
`--agent` role file, which is applied last. A physical file reached more than once, for
example a role that is a symlink or hard link to the global or primary file, is read once and
applied only at its last position. The role therefore always wins over the files beneath it.

`--no-extend` skips the separate global layer in both the TUI and headless runs. Built-in
defaults still apply, and a primary file that is itself the global file is still read.

Merge rules:
- Tables merge by key: `[ui]`, `[shell]`, `[env]`, `[providers]` (per provider name) and
  `[hooks]` (per hook name). A layer sets only the fields it writes.
- Scalars replace inherited values, including `""`, `0` and `false`.
- Arrays replace inherited arrays completely (`status_left`, `status_right`, hook `events`).
- `system` replaces the whole system prompt; `system = ""` clears it.
- `enabled = false` disables an inherited hook with the same name.
- A provider's `api_key` and `api_key_env` are checked after merging. To drop an inherited
  one, set it to `""` in the higher layer.
- Generic string interpolation, profiles and include directives are not supported.

Validation runs on the merged result. Unknown keys and unknown tables are errors, so a typo
stops startup. Error messages are listed in `dashy docs troubleshooting`.

## Writes from the TUI

`/model` saves provider, model and effort to the file that supplies the model setting: the
highest layer that sets `model`, which may be the inherited global file or the `--agent` role
file. Only the selection keys are written there; nothing is copied from other layers. If no
layer sets `model`, the primary file is used. The notice names the file that was written. If a
higher layer also sets provider, model or effort, the selection is rejected with the name of
that file, because saving would not change the effective defaults.

`/login` always writes the primary file, never the role file. It patches raw TOML keys and
keeps unrelated comments, values and sparse inheritance. It cannot overwrite an existing
provider name; edit the file to change one. Credentials saved into a file inside the project
directory produce a warning. The global file is never treated as a project file.

Writes take an exclusive lock on a `.lock` file beside the target, so concurrent Dashy
processes do not lose each other's changes: `dashy.toml.lock` for a `dashy.toml`, and
`reviewer.toml.lock` for a role file named `reviewer.toml`. Lock files are left in place. Ignore
them yourself, for example by adding `/dashy.toml.lock` or `roles/reviewer.toml.lock` to `.gitignore`. Writes
follow symlinks, so a symlinked `dashy.toml` stays a symlink. Files are written with `0600`
permissions. A write that fails validation leaves the file and the session unchanged.

## Reloading

`/reload` in the TUI re-reads the files for the active chat. Settings, `[env]`, hooks and
status commands take effect for later tool calls and events. The active provider, model and
effort stay as selected in the session. `/new` and `/resume` also re-read the files. A `/resume`
that fails leaves the current session, its hooks and its `[env]` unchanged.

## The dashy.toml Format

Built-in defaults (the exact file shipped inside this binary; every key you omit keeps the
value shown here):

```toml
{{defaults}}
```

Write only the keys you want to change. A minimal project file is shown in `dashy docs cookbook`.

## Key Reference

Keys under a table are shown with their table prefix: `ui.max_code_lines` is `max_code_lines`
inside `[ui]`. Sizes in bytes and durations in seconds are plain integers.

| Key | Type | Default | Constraints |
| --- | --- | --- | --- |
| `provider` | string | `""` | Empty, or the name of a `[providers.NAME]` table. |
| `model` | string | `""` | Model name sent to the provider. Empty makes requests fail with "no provider or model selected". |
| `effort` | string | `""` | Empty, or one of `none`, `minimal`, `low`, `medium`, `high`, `xhigh`, `max`. |
| `reasoning_summary` | string | `"auto"` | `auto` requests reasoning summaries; `off` does not. |
| `max_context_tokens` | integer | `400000` | At least 0. Estimated input tokens above which compaction runs before a request; `0` disables it. |
| `compact_keep_turns` | integer | `4` | At least 0. Recent turns kept verbatim when compacting. |
| `max_agent_steps` | integer | `0` | At least 0. Tool iterations allowed per turn; `0` is unlimited. |
| `max_file_bytes` | integer | `65536` | At least 0. Per-file byte limit for `@path` attachments; `0` is unlimited. |
| `attachment_dir` | string | `""` | Where captured images are saved. Empty uses `dashy-images` in the platform temporary directory for TUI clipboard pastes, and `$DASHY_HOME/attachments` for headless `images`. `dashy view` always uses `$DASHY_HOME/attachments` and ignores this key. Relative paths resolve against the launch directory. |
| `system` | string | `"You are Dashy, a coding agent"` | System prompt. `""` clears it. |
| `ui.max_code_lines` | integer | `999` | At least 0. Visible body rows of code blocks; `0` hides the body. |
| `ui.max_result_lines` | integer | `4` | At least 0. Visible body rows of tool output; `0` hides the body. |
| `ui.max_thinking_lines` | integer | `4` | At least 0. Visible body rows of reasoning; `0` hides the body. |
| `ui.status_left` | array of strings | two built-in snippets | Each entry is a shell snippet printing one segment. `[]` hides the side. |
| `ui.status_right` | array of strings | one built-in snippet | Same rules as `status_left`. |
| `shell.timeout` | integer seconds | `120` | Greater than 0 and not above `shell.max_timeout`. Default for bash tool calls. |
| `shell.max_timeout` | integer seconds | `600` | Greater than 0. Largest timeout a call may request. |
| `shell.max_output` | integer bytes | `8192` | At least 0. Output bytes kept per call; `0` disables truncation. |
| `env.NAME` | string | none | NAME must not start with `DASHY_`. |
| `providers.NAME.base_url` | string | none | HTTP(S) URL with a host, when set. The provider must speak the OpenAI Responses API. |
| `providers.NAME.api_key` | string | none | Literal secret. Cannot be combined with a nonempty `api_key_env`. |
| `providers.NAME.api_key_env` | string | none | Name of the variable holding the key, read from `[env]` and then the process environment. Must be set and nonblank while this provider is selected. Cannot be combined with a nonempty `api_key`. |
| `hooks.NAME.events` | array of strings | none | Required unless the hook is disabled. Event names from `dashy docs hooks`, or `"*"`. |
| `hooks.NAME.command` | string | none | Required unless the hook is disabled. Non-blank Bash command. |
| `hooks.NAME.timeout` | integer seconds | `10` | 1 to 600. `0` or omitted means 10. |
| `hooks.NAME.enabled` | boolean | `true` | `false` disables the hook, including an inherited one with the same name. |
| `hooks.NAME.send_output` | boolean | `false` | `true` queues captured output for the next turn. |
| `hooks.NAME.headless_support` | boolean | `true` | `false` skips the hook in `--mode jsonl` runs. |

Notes on the tables:
- `[ui]` status commands run with the DASHY_* metadata described in `dashy docs cli`. Their
  environment, limits and refresh rules are in `dashy docs interface`.
- `[shell]` values are shown to the model in the bash tool schema. Shell behavior is in `dashy docs runtime`.
- `[env]` values are applied separately to every shell call, hook and status command. Dashy
  never changes its own process environment.
- Hook fields are listed above. Events, delivery, variables and loop prevention are in `dashy docs hooks`.
  Headless runs load hooks the same way as the TUI, except hooks with `headless_support = false`.
- Secrets are never copied into the file, session history or logs. `api_key_env` names a
  variable. The key itself stays in the environment.

Example provider table:

```toml
[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"
```
