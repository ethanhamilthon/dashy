# Dashy documentation

Run `dashy docs TOPIC` to read a topic:
- config: dashy.toml discovery and precedence, merge rules, and every key with its type, default and constraints.
- cli: every command, flag, exit code, and the DASHY_* environment variable table.
- cookbook: short recipes (minimal config, unattended limits, Python headless sub-agents, long jobs).
- runtime: the bash tool in detail (stateless shell, output and timeout limits, background work).
- hooks: named hooks, lifecycle events and their variables, send_output delivery, loop prevention.
- headless: the --mode jsonl request/event transport, its operations, dashy notify, and session ownership.
- sessions: storage layout, session commands, and locks.
- interface: slash commands, @ file completion and references, the padded status bar, and keys.
- models: model and effort selection, /login, provider secrets, and retries.
- images: dashy view and queued image delivery.
- clipboard: image paste, attachment labels, shortcuts, and retention.
- steering: instructions queued while the agent works, acceptance, and recall.
- troubleshooting: startup and configuration errors with their exact messages, and log files.
- update: dashy update, the startup update check, and update.json.

Each fact is documented in one topic; other topics point to it.
These topics describe implemented behavior, not the planned TODO backlog.
