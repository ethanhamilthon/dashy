# Cookbook: common tasks

Short recipes. Each recipe names the topic that owns the details.

## Create a minimal project config

Put `dashy.toml` in the project root (see `dashy docs config` for discovery). Write only what
differs from the defaults:

```toml
provider = "openrouter"
model = "anthropic/claude-sonnet-4"
effort = "medium"

[providers.openrouter]
base_url = "https://openrouter.ai/api/v1"
api_key_env = "OPENROUTER_API_KEY"   # prefer this over a literal api_key in a repo
```

The provider must speak the OpenAI Responses API. A typo in a key name stops startup with an
error; see `dashy docs troubleshooting`.

## Tighten a config for unattended runs

```toml
max_agent_steps = 60        # 0 means unlimited
[shell]
timeout = 60
max_timeout = 300
```

## Run dashy headless from Python (sub-agents)

`dashy --mode jsonl` is the only non-TUI mode: JSONL in, JSONL out (`dashy docs headless`).
Keep stdin open while work runs: send `input`, wait for its terminal `done`, then
send `quit` before closing stdin. For sub-agents, save this wrapper as `subagent.py`:

```python
#!/usr/bin/env python3
"""Run dashy sub-agents headless. Usage: subagent.py CONFIG PROMPT [PROMPT...]"""
import json
import subprocess
import sys
from concurrent.futures import ThreadPoolExecutor


def run_subagent(prompt, config, cwd=".", resume=None):
    """One turn in a fresh session (or --resume ID). Returns (answer, session_id)."""
    cmd = ["dashy", "--mode", "jsonl", "--config", config, "--no-extend"]
    if resume:
        cmd += ["--resume", resume]
    answer, session_id, status, problem = "", resume, None, ""
    # Inherit stderr so diagnostics cannot fill an unread pipe.
    with subprocess.Popen(cmd, cwd=cwd, stdin=subprocess.PIPE,
                          stdout=subprocess.PIPE, text=True) as proc:
        proc.stdin.write(json.dumps({"id": "1", "type": "input", "text": prompt}) + "\n")
        proc.stdin.flush()
        quitting = False
        for line in proc.stdout:
            record = json.loads(line)
            kind = record["type"]
            if kind == "ready":
                session_id = record["state"]["session"]["id"]
            elif kind == "event" and record["event"]["kind"] == "assistant" \
                    and not record["event"].get("provisional"):
                answer = record["event"]["text"]
            elif kind == "done":
                status, problem = record["status"], record.get("error", "")
            elif kind == "result" and record.get("id") == "1" and not record["ok"]:
                status, problem = "error", record["error"]["message"]
            if status is not None and not quitting:
                proc.stdin.write(json.dumps({"id": "2", "type": "quit"}) + "\n")
                proc.stdin.flush()
                proc.stdin.close()
                quitting = True
        # Read through shutdown and wait for the process to finish saving.
    if proc.returncode != 0 or status != "ok":
        raise RuntimeError(f"sub-agent failed (exit {proc.returncode}, status {status}): {problem}")
    return answer, session_id


if __name__ == "__main__":
    config, prompts = sys.argv[1], sys.argv[2:]
    with ThreadPoolExecutor(max_workers=len(prompts)) as pool:  # each gets its own session
        for answer, _ in pool.map(lambda p: run_subagent(p, config), prompts):
            print(answer)
```

Notes:
- Use a dedicated config (`sub.toml`) with `--no-extend`; set `max_agent_steps` and
  `[shell]` limits there. Hooks from this config run, except those with `headless_support = false`.
- To keep a conversation, pass the returned `session_id` as `resume` next time.
  One process may own a session at a time ("session is in use").
- For a long conversation, keep the process open instead: send several
  `input` requests to its stdin and read records until each turn's `done`. Send
  `quit` when finished; closing stdin is not the normal completion handshake.
- Call it from the bash tool in the background and report back:
  `(python3 subagent.py sub.toml 'Refactor pkg/x' > /tmp/sub.out 2>&1; dashy notify "sub-agent done: $(tail -n 5 /tmp/sub.out)") &`
  The script inherits this session's `DASHY_ENDPOINT`, so `dashy notify` reaches
  the parent even though the child dashy has its own session.

## Run a long job and get called back

From the bash tool, redirect output and notify when done:

```bash
(make release > /tmp/release.log 2>&1; dashy notify "release: exit $?") &
```

## Run tests after each user turn

Add a `turn_end` hook that runs only for user turns and calls `dashy notify` on failure. The full
example, the loop guard and the caveats are in `dashy docs hooks`.

## Change the status bar

Set `status_left` or `status_right` under `[ui]`. An array replaces the defaults entirely, and `[]`
hides a side. Each entry is a shell snippet that prints one segment. Details: `dashy docs interface`.

## Apply config changes

Edit the file, then run `/reload` in the TUI. Precedence and merge rules: `dashy docs config`.
