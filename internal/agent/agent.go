package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"

	"dashy/internal/shell"
)

const BashToolDescription = `Run a command in system Bash (/bin/bash --noprofile --norc).
Each call starts a fresh process in the project directory; variables, functions,
aliases and the working directory do not persist between calls. There is no TTY
and stdin is empty. Programs run from PATH with the user's permissions.

Result: {output, exit_code}. Stdout and stderr are merged.
timeout: seconds, null = 120, maximum 600. A timeout (exit 124) or cancellation
(exit 130) kills the whole process group.
max_output: bytes kept, null = 8192, 0 = no truncation. When output is cut, the
middle is removed and the first and last halves are kept.
Prefer filtering: rg, grep, head, tail, sed -n, wc. Create files with a quoted heredoc.
Background processes do not outlive the call; for long work redirect output to a
file and run ` + "`dashy notify`" + ` when it finishes.
This shell is not a sandbox. Do not do destructive things unless asked.

You are running inside dashy, a minimal coding-agent harness. Your instructions are
the AGENTS.md files you were given as the system prompt (~/.dashy/AGENTS.md, then
the project's AGENTS.md or the file chosen with --config). A ` + "```dashy" + ` fenced block
in those files was already run when the session started; its output is part of
the prompt. Run ` + "`dashy --help`" + ` for the full reference.

dashy commands you can run from the shell:
- dashy -p "TASK" [--config FILE] [--no-global]
    Run an independent sub-agent and print only its final answer to stdout
    (progress goes to stderr; exit code is non-zero if it failed). It does not
    see this conversation, so put everything it needs into TASK. Use --config
    to give it a different AGENTS.md (a different role) and --no-global to drop
    ~/.dashy/AGENTS.md. Text piped to stdin is appended to TASK.
- dashy notify TEXT...
    Send a message to this session. An idle session starts a turn; a busy one
    receives it as a steering message. Needs DASHY_ENDPOINT (set in this shell).
- dashy view PATH...
    Attach PNG/JPEG images to your next model request (up to four).
- dashy --help
    Reference for flags, AGENTS.md and the dashy block.

Every call has DASHY_SESSION_ID and DASHY_ENDPOINT set.`

var codeField = regexp.MustCompile(`"command"\s*:\s*"`)

// partialCode decodes only the complete prefix of a JSON string, withholding split
// escapes, UTF-8 characters and surrogate pairs until the next delta completes them.
func partialCode(arguments string) string {
	loc := codeField.FindStringIndex(arguments)
	if loc == nil {
		return ""
	}
	quoted := arguments[loc[1]-1:]
	var text string
	if json.NewDecoder(strings.NewReader(quoted)).Decode(&text) == nil {
		return text
	}
	for end := len(quoted); end > 0 && end >= len(quoted)-12; end-- {
		candidate := quoted[:end]
		if !utf8.ValidString(candidate) {
			continue
		}
		if json.Unmarshal([]byte(candidate+`"`), &text) == nil {
			return text
		}
	}
	return ""
}

// Options selects the configuration and session of an agent.
type Options struct {
	Config   string // --config FILE, relative to the project directory
	NoGlobal bool   // --no-global
	Resume   string // --resume ID; "" starts a new session
	Print    bool   // -p: no session socket, DASHY_MODE=print
}

// Agent holds one session: its history, settings, prompt and ownership lock.
type Agent struct {
	opts         Options
	home         string // DASHY_HOME
	settings     Settings
	client       openai.Client
	Shell        *shell.Runner
	History      []json.RawMessage
	HistoryPath  string
	sessionsDir  string
	owner        *os.File
	promptFiles  []string // AGENTS.md files selected at startup, /new or /resume
	prompt       string   // assembled system prompt, frozen once assembled
	promptFrozen bool
	promptSource string // startup, new or resume (DASHY_SOURCE)
	records      []Record
	title        string // custom title set by /rename; "" means the first user message
	lastRequest  Metadata
	listener     net.Listener
	socketPath   string
	inbox        chan string
	views        chan shell.Image
	slots        *imageSlots // shared allowance of new images per model request
}

// New opens the project in dir. Options.Resume optionally resumes a saved session.
func New(dir string, opts Options) (*Agent, error) {
	home, err := DashyHome(dir)
	if err != nil {
		return nil, err
	}
	a := &Agent{opts: opts, home: home, promptSource: "startup", Shell: &shell.Runner{Dir: dir}, slots: &imageSlots{}}
	a.Shell.Env = a.shellEnv
	if err := a.loadConfig(dir); err != nil {
		return nil, err
	}
	_ = CleanAttachments(a.AttachmentsDir())
	if err := a.openProject(dir); err != nil {
		a.release()
		return nil, err
	}
	return a, nil
}

// loadConfig reads settings and selects the AGENTS.md files for the project in dir.
func (a *Agent) loadConfig(dir string) error {
	settings, err := LoadSettings(a.SettingsPath())
	if err != nil {
		return err
	}
	files, err := instructionFiles(a.home, dir, a.opts)
	if err != nil {
		return err
	}
	a.applySettings(settings)
	a.promptFiles = files
	return nil
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

func (a *Agent) message(role, text string) {
	a.History = append(a.History, raw(map[string]any{"role": role, "content": text}))
}

// Save writes the session file atomically.
func (a *Agent) Save() error {
	if err := os.MkdirAll(filepath.Dir(a.HistoryPath), 0700); err != nil {
		return err
	}
	data, err := a.sessionData()
	if err != nil {
		return err
	}
	return writePrivate(a.HistoryPath, data)
}

// Run is RunInput for a text message.
func (a *Agent) Run(ctx context.Context, text string, emit func(Event), steering ...*Steering) error {
	return a.RunInput(ctx, Input{Text: text}, emit, steering...)
}

// RunInput runs one turn: the input, then model requests and bash calls until the
// model stops. Events describe display changes; history is saved as it grows.
func (a *Agent) RunInput(ctx context.Context, input Input, emit func(Event), steering ...*Steering) (err error) {
	if a.records == nil {
		a.records = a.Records()
	}
	forward := emit
	emit = func(e Event) {
		if e.Kind != "reset" { // reset already rebuilt a.records from the history
			a.records = ApplyEvent(a.records, e)
		}
		forward(e)
	}

	defer func() {
		err = a.safeError(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			emit(Event{Kind: "error", Text: err.Error()})
		}
		if a.HasSavedSession() {
			if e := a.Save(); e != nil {
				saveError := fmt.Errorf("could not save history: %w", e)
				err = errors.Join(err, saveError)
				emit(Event{Kind: "error", Text: saveError.Error()})
			}
		}
	}()

	// A crash may leave calls without outputs. Their effects are unknown, so
	// never rerun them; supply an explicit interrupted result on continuation.
	for _, output := range interruptedOutputs(a.History) {
		a.History = append(a.History, output)
		for _, r := range RecordsForItem(output) {
			emit(Event{Kind: r.Kind, Text: r.Text, Final: true})
		}
	}

	// Only the turn goroutine mutates history. Steering waits until all outputs
	// for the current response have been committed, preserving call/output pairs.
	acceptSteering := func() bool {
		if ctx.Err() != nil {
			return false
		}
		var instructions []Input
		if len(steering) > 0 {
			instructions = steering[0].TakeInputs()
		}
		for _, text := range a.Notifications() {
			instructions = append(instructions, Input{Text: text})
		}
		accepted := false
		for _, instruction := range instructions {
			if strings.TrimSpace(instruction.Text) != "" || len(instruction.Images) > 0 {
				a.addInput(instruction, emit, true)
				accepted = true
			}
		}
		return accepted
	}

	shown := inputEvent(input, false)
	shown.ID = fmt.Sprintf("input-%d", time.Now().UnixNano())
	emit(shown)
	a.commitInput(input)

	turnStart := time.Now()
	var total Usage
	usageKnown := true
	for step := 0; ; step++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		acceptSteering()
		if images := a.viewed(); len(images) > 0 {
			text := "<notify>\nImages queued by dashy view." + imageOriginals(images) + "\n</notify>"
			content := []any{map[string]any{"type": "input_text", "text": text}}
			for _, img := range images {
				content = append(content, map[string]any{"type": "input_image", "image_url": img.URL, "detail": "auto"})
			}
			a.History = append(a.History, raw(map[string]any{"role": "user", "content": content}))
			emit(Event{Kind: "notice", Text: text})
		}
		a.compactIfNeeded(ctx, emit)
		if e := a.Save(); e != nil {
			return fmt.Errorf("could not save history: %w", e)
		}
		baseHistory := append([]json.RawMessage(nil), a.History...)
		baseRecords := append([]Record(nil), a.records...)
		finished := map[int64]json.RawMessage{}
		var durableHistory []json.RawMessage
		checkpoint := func(index int64, item json.RawMessage) error {
			finished[index] = append(json.RawMessage(nil), item...)
			indexes := make([]int, 0, len(finished))
			for i := range finished {
				indexes = append(indexes, int(i))
			}
			sort.Ints(indexes)
			durableHistory = append([]json.RawMessage(nil), baseHistory...)
			for _, i := range indexes {
				durableHistory = append(durableHistory, finished[int64(i)])
			}
			snapshot := *a
			snapshot.History = durableHistory
			if err := snapshot.Save(); err != nil {
				return fmt.Errorf("could not save history: %w", err)
			}
			return nil
		}
		items, e := a.requestWithRetries(ctx, emit, checkpoint)
		if e != nil {
			if durableHistory != nil {
				a.History = durableHistory
			} else {
				a.records = baseRecords
			}
			return e
		}
		// Discard streaming-only entries from the persistent journal.
		a.records = baseRecords
		meta := a.lastRequest
		if meta.Usage == nil {
			usageKnown = false
		} else {
			total.Input += meta.Usage.Input
			total.Output += meta.Usage.Output
			if total.Cached < 0 || meta.Usage.Cached < 0 {
				total.Cached = -1
			} else {
				total.Cached += meta.Usage.Cached
			}
		}
		calls := 0
		// Commit and display all response items before executing their tool calls.
		for i, item := range items {
			a.History = append(a.History, item)
			for _, r := range RecordsForItem(item) {
				m := meta
				if r.Kind == "assistant" {
					m.Duration = time.Since(turnStart)
					m.Usage = nil
					if usageKnown {
						u := total
						m.Usage = &u
					}
				}
				emit(Event{Kind: r.Kind, Text: r.Text, ID: a.itemID(int64(i)), Meta: &m, Final: true})
			}
		}
		if e := a.Save(); e != nil {
			return fmt.Errorf("could not save history: %w", e)
		}
		for _, item := range items {
			var call struct {
				Type, Name, Arguments string
				CallID                string `json:"call_id"`
			}
			if e = json.Unmarshal(item, &call); e != nil {
				return e
			}
			if call.Type != "function_call" {
				continue
			}
			calls++
			started := time.Now()
			result := a.runTool(ctx, call.Name, call.Arguments)
			output := raw(map[string]any{"type": "function_call_output", "call_id": call.CallID, "output": string(raw(result))})
			a.History = append(a.History, output)
			for _, r := range RecordsForItem(output) {
				emit(Event{Kind: r.Kind, Text: r.Text, Meta: &Metadata{Duration: time.Since(started)}, Final: true})
			}
			if e := a.Save(); e != nil {
				return fmt.Errorf("could not save history: %w", e)
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if steered := acceptSteering(); calls == 0 && !steered {
			return nil
		} else if steered {
			if e := a.Save(); e != nil {
				return fmt.Errorf("could not save history: %w", e)
			}
		}
	}
}

// runTool executes one function call; failures become an error result for the model.
func (a *Agent) runTool(ctx context.Context, name, arguments string) shell.Result {
	if ctx.Err() != nil {
		return shell.Result{Output: ctx.Err().Error(), Error: true}
	}
	if name != "bash" {
		return shell.Result{Output: "Unknown tool: " + name, Error: true}
	}
	var args struct {
		Command   string `json:"command"`
		Timeout   *int64 `json:"timeout"`
		MaxOutput *int64 `json:"max_output"`
	}
	timeout := time.Duration(ShellTimeout) * time.Second
	limit := ShellOutput
	err := json.Unmarshal([]byte(arguments), &args)
	if err == nil && args.Timeout != nil {
		if *args.Timeout <= 0 || *args.Timeout > ShellMaxTimeout {
			err = fmt.Errorf("timeout must be 1..%d seconds", ShellMaxTimeout)
		} else {
			timeout = time.Duration(*args.Timeout) * time.Second
		}
	}
	if err == nil && args.MaxOutput != nil {
		if *args.MaxOutput < 0 || *args.MaxOutput > int64(math.MaxInt32) {
			err = fmt.Errorf("max_output must be >= 0 bytes (0 = no truncation)")
		} else {
			limit = int(*args.MaxOutput)
		}
	}
	if err == nil {
		var result shell.Result
		if result, err = a.Shell.Execute(ctx, args.Command, timeout, limit); err == nil {
			return result
		}
	}
	return shell.Result{Output: err.Error(), ExitCode: 2, Error: true}
}

// itemID identifies a response item of the latest request in display events.
func (a *Agent) itemID(index int64) string {
	return fmt.Sprintf("%s-%d", a.lastRequest.RequestID, index)
}

// request sends the history and returns the completed response items.
func (a *Agent) request(ctx context.Context, emit func(Event), checkpoints ...func(int64, json.RawMessage) error) (items []json.RawMessage, err error) {
	defer func() { err = a.safeError(err) }()
	if strings.TrimSpace(a.settings.Model) == "" || strings.TrimSpace(a.settings.Provider) == "" {
		return nil, fmt.Errorf("no provider or model selected: use /settings to choose a model (or /login to add a provider)")
	}
	prompt, err := a.instructions(ctx)
	if err != nil {
		return nil, err
	}
	input := make(responses.ResponseInputParam, len(a.History))
	for i, item := range a.History {
		if !json.Valid(item) {
			return nil, fmt.Errorf("invalid history item %d", i)
		}
		input[i] = param.Override[responses.ResponseInputItemUnionParam](item)
	}
	payload := responses.ResponseNewParams{
		Model:        a.settings.Model,
		Instructions: openai.String(prompt),
		Input:        responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		Store:        openai.Bool(false),
		Tools: []responses.ToolUnionParam{{OfFunction: &responses.FunctionToolParam{
			Name:        "bash",
			Description: openai.String(BashToolDescription),
			Parameters:  bashParameters(),
			Strict:      openai.Bool(true),
		}}},
	}
	if a.settings.Effort != "" {
		payload.Reasoning.Effort = shared.ReasoningEffort(a.settings.Effort)
	}
	if a.settings.ReasoningSummary {
		payload.Reasoning.Summary = shared.ReasoningSummaryAuto
	}
	started := time.Now()
	a.lastRequest = Metadata{RequestID: fmt.Sprintf("r-%d", len(a.History))}
	stream := a.client.Responses.NewStreaming(ctx, payload)
	defer stream.Close()
	var completed []json.RawMessage
	arguments := map[int64]string{}
	names := map[int64]string{}
	for stream.Next() {
		event := stream.Current()
		switch event.Type {
		case "response.function_call_arguments.delta", "response.function_call_arguments.done":
			if event.Type == "response.function_call_arguments.delta" {
				arguments[event.OutputIndex] += event.Delta
			} else {
				arguments[event.OutputIndex] = event.Arguments
			}
			if names[event.OutputIndex] == "bash" {
				emit(Event{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: a.itemID(event.OutputIndex), Final: true})
			}
		case "response.output_item.added", "response.output_item.done":
			if event.Type == "response.output_item.done" && len(checkpoints) > 0 && checkpoints[0] != nil {
				if err := checkpoints[0](event.OutputIndex, json.RawMessage(event.Item.RawJSON())); err != nil {
					return nil, err
				}
			}
			// Some providers deliver complete arguments without argument deltas.
			if event.Item.Type == "function_call" {
				names[event.OutputIndex] = event.Item.Name
				if event.Item.Arguments.OfString != "" {
					arguments[event.OutputIndex] = event.Item.Arguments.OfString
				}
				if event.Item.Name == "bash" {
					emit(Event{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: a.itemID(event.OutputIndex), Final: true})
					continue
				}
			}
			// Display of a finished item is emitted by RunInput when the response commits.
			if event.Type == "response.output_item.added" {
				for _, record := range RecordsForItem(json.RawMessage(event.Item.RawJSON())) {
					emit(Event{Kind: record.Kind, Text: record.Text, ID: a.itemID(event.OutputIndex), Final: true, Provisional: true})
				}
			}
		case "response.output_text.delta":
			emit(Event{Kind: "delta", Text: event.Delta, ID: a.itemID(event.OutputIndex), Provisional: true})
		case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
			emit(Event{Kind: "reasoning_delta", Text: event.Delta, ID: a.itemID(event.OutputIndex), Provisional: true})
		case "response.refusal.delta":
			emit(Event{Kind: "refusal_delta", Text: event.Delta, ID: a.itemID(event.OutputIndex), Provisional: true})
		case "response.completed":
			a.lastRequest.Duration = time.Since(started)
			if event.Response.JSON.Usage.Valid() {
				n := event.Response.Usage.InputTokens
				a.lastRequest.ContextInput = &n
				cached := int64(-1)
				if event.Response.Usage.InputTokensDetails.JSON.CachedTokens.Valid() {
					cached = event.Response.Usage.InputTokensDetails.CachedTokens
				}
				a.lastRequest.Usage = &Usage{Input: n, Output: event.Response.Usage.OutputTokens, Cached: cached}
			}
			completed = make([]json.RawMessage, len(event.Response.Output))
			for i, item := range event.Response.Output {
				completed[i] = json.RawMessage(item.RawJSON())
			}
		case "response.failed", "response.incomplete", "error":
			return nil, streamFailure(event)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, fmt.Errorf("provider stream ended without a completed response")
	}
	return completed, nil
}

// ContextTokens is the size of the next prompt: the provider's count for the latest
// request, or an estimate from the history when no usage is known (resumed sessions).
func (a *Agent) ContextTokens() int64 {
	if a.lastRequest.ContextInput != nil {
		return *a.lastRequest.ContextInput
	}
	return estimateTokens(a.History)
}

// FinalText is the assistant text of the latest response: the message items after
// the last tool output, within the current turn. Only committed history is read.
func (a *Agent) FinalText() string {
	var texts []string
	for i := len(a.History) - 1; i >= 0; i-- {
		var v struct {
			Role, Type string
		}
		if json.Unmarshal(a.History[i], &v) != nil || v.Role == "user" || v.Type == "function_call" || v.Type == "function_call_output" {
			break
		}
		for _, r := range RecordsForItem(a.History[i]) {
			if r.Kind == "assistant" && r.Text != "" {
				texts = append([]string{r.Text}, texts...)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// PromptFiles lists the AGENTS.md files selected for the session, in prompt order.
func (a *Agent) PromptFiles() []string { return append([]string(nil), a.promptFiles...) }

// SessionID is the ID of the active session.
func (a *Agent) SessionID() string {
	return strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
}

// bashParameters is the strict JSON schema of the bash tool.
func bashParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"timeout": map[string]any{
				"type":        []string{"integer", "null"},
				"description": fmt.Sprintf("seconds, default %d, maximum %d", ShellTimeout, ShellMaxTimeout),
			},
			"max_output": map[string]any{
				"type":        []string{"integer", "null"},
				"description": fmt.Sprintf("output bytes to keep, default %d (first half + last half); 0 = no truncation", ShellOutput),
			},
		},
		"required":             []string{"command", "timeout", "max_output"},
		"additionalProperties": false,
	}
}
