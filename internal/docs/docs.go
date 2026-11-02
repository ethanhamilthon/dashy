// Package docs serves the built-in documentation printed by `dashy docs` and
// the bash tool description that is sent to the model.
package docs

import (
	"embed"
	"fmt"
	"io"
	"strings"

	"dashy/internal/defaults"
)

// ToolDescription is the bash tool contract sent to the model. It has two parts:
// the short shell rules, then a block that tells the agent what Dashy is and
// which topic owns each subject. Everything longer lives in the topics.
const ToolDescription = `Run a command in system Bash (/bin/bash --noprofile --norc). Each call starts a
fresh process in the project directory; variables, functions, aliases and working
directory changes do not persist between calls. External programs run from PATH
with the user's permissions. There is no TTY and stdin is empty.
Result: {output, exit_code}. Stdout and stderr are merged. max_output: bytes kept
(null uses the configured default; 0 disables truncation; the middle is cut with a notice).
timeout: seconds (null uses the configured default; the schema states the maximum).
A timeout (exit 124) or cancellation (exit 130) kills the command's process group.
Prefer filtering with rg, grep, head, tail, sed -n or wc. Create files with a quoted heredoc.
Long-running work: redirect stdout and stderr to a file and notify on completion.
Background processes are not persistent session jobs.
Dashy commands (ordinary programs, also usable from scripts and background jobs):
- dashy notify TEXT... (or stdin): send a <notify> message to this session. An idle agent starts a turn; a busy one receives it as steering.
- dashy view PATH...: queue PNG/JPEG images for the next model request (at most four wait).
- dashy docs [TOPIC]: print built-in documentation.
Every call exports DASHY_SESSION_ID, DASHY_MODEL, DASHY_PROVIDER, DASHY_EFFORT,
DASHY_CTX_SIZE (latest request input tokens, empty if unknown) and DASHY_ENDPOINT
(session socket). The full variable list is in the cli topic.
The shell is not an OS security sandbox; do not perform destructive actions unless requested.

About Dashy
Dashy is the minimal coding-agent harness that runs you. Its behavior is set ONLY by dashy.toml:
- Project file: ./dashy.toml in the launch directory (no upward search). Global file: $DASHY_HOME/dashy.toml (default ~/.dashy/dashy.toml). --config PATH selects the primary file instead.
- Files merge over built-in defaults; unknown keys fail startup. Apply edits with /reload in the TUI (the active provider, model and effort are kept).
- BEFORE editing dashy.toml or answering questions about Dashy, run: dashy docs config (every key, default, constraint, and the hooks pointer).
- dashy docs index lists all topics. Key topics:
  config: precedence, merge rules, keys
  hooks: lifecycle events, hook behavior, delivery
  cli: commands, flags, DASHY_* variables, exit codes
  cookbook: recipes, sub-agents, headless driver
  headless: the --mode jsonl protocol
  runtime: shell tool details
  sessions: storage, commands, locks
  troubleshooting: startup and config errors, log files
  update: dashy update and the update check`

//go:embed docs/*.md
var documentation embed.FS

// Docs prints a documentation topic for dashy docs.
func Docs(out io.Writer, args []string) error {
	topic := "index"
	if len(args) > 1 {
		return fmt.Errorf("usage: docs [TOPIC]")
	} else if len(args) == 1 {
		topic = args[0]
	}
	data, err := documentation.ReadFile("docs/" + topic + ".md")
	if topic == "" || strings.ContainsAny(topic, "/\\.") || err != nil {
		return fmt.Errorf("unknown topic %q; run docs index", topic)
	}
	// The shipped defaults are embedded once; docs never carry a copy of them.
	text := strings.ReplaceAll(string(data), "{{defaults}}", strings.TrimSpace(defaults.Config))
	_, err = io.WriteString(out, text)
	return err
}
