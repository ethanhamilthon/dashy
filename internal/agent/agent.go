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

const bashToolDescription = BashToolDescription

var codeField = regexp.MustCompile(`"command"\s*:\s*"`)

// Decode only the complete prefix of a JSON string, withholding split escapes,
// UTF-8 characters and surrogate pairs until the next delta completes them.
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
			if len(candidate) >= 6 {
				tail := strings.ToLower(candidate[len(candidate)-6:])
				if strings.HasPrefix(tail, `\ud`) && strings.ContainsAny(tail[3:4], "89ab") {
					slashes := 0
					for i := len(candidate) - 6; i >= 0 && candidate[i] == '\\'; i-- {
						slashes++
					}
					if slashes%2 == 1 {
						continue
					}
				}
			}
			return text
		}
	}
	return ""
}

type Agent struct {
	listener         net.Listener
	socketPath       string
	NoSocket         bool
	inbox            chan string
	views            chan shell.Image
	owner            *os.File
	settings         Settings
	Shell            *shell.Runner
	History          []json.RawMessage
	HistoryPath      string
	System           string
	LocalConfig      string
	Sources          []Source
	primaryConfig    string
	client           openai.Client
	records          []Record
	title            string // set by /rename; "" means the first user message
	lastRequest      Metadata
	files            ConfigFiles
	home             string
	sessionsDir      string
	version          string
	promptFrozen     bool
	promptSource     string
	mode             string
	compactKeepTurns int
	compactThreshold int64
}

// New opens the project in dir with configuration from files (zero value: default
// lookup). target optionally resumes a saved session id.
func New(dir string, files ConfigFiles, target ...string) (*Agent, error) {
	config, err := configDir(dir)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfiguration(config, dir, files)
	if err != nil {
		return nil, err
	}
	mode := files.Mode
	if mode == "" {
		mode = "tui"
	}
	source := "startup"
	if len(target) > 0 && target[0] != "" && target[0] != "new" {
		source = "resume"
	}
	a := &Agent{
		files:         files,
		home:          config,
		mode:          mode,
		promptSource:  source,
		NoSocket:      mode == "print",
		System:        cfg.system,
		LocalConfig:   cfg.local,
		Sources:       cfg.sources,
		primaryConfig: cfg.primary,
		Shell:         &shell.Runner{Dir: dir},
	}
	a.applySettings(cfg.settings)
	_ = CleanAttachments(filepath.Join(config, "attachments"))
	if err := a.openProject(dir, target...); err != nil {
		a.release()
		return nil, err
	}
	a.Shell.Env = a.shellEnv
	return a, nil
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }
func (a *Agent) message(role, text string) {
	a.History = append(a.History, raw(map[string]any{"role": role, "content": text}))
}

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

func (a *Agent) Run(ctx context.Context, text string, emit func(Event), steering ...*Steering) error {
	return a.RunInput(ctx, Input{Text: text}, emit, steering...)
}

func (a *Agent) RunInput(ctx context.Context, input Input, emit func(Event), steering ...*Steering) (err error) {
	if a.records == nil {
		a.records = a.Records()
	}
	forward := emit
	emit = func(e Event) {
		a.records = ApplyEvent(a.records, e)
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
		accepted := false
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
		compacted := len(a.History)
		a.compactIfNeeded(ctx, emit)
		if len(a.History) < compacted {
			a.records = a.Records()
		}
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
				part := finished[int64(i)]
				durableHistory = append(durableHistory, part)
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
					if usageKnown {
						u := total
						m.Usage = &u
					} else {
						m.Usage = nil
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
	timeout := time.Duration(DefaultShellTimeout) * time.Second
	maxTimeout := time.Duration(DefaultShellMaxTimeout) * time.Second
	limit := DefaultShellMaxOutput
	err := json.Unmarshal([]byte(arguments), &args)
	if err == nil && args.Timeout != nil {
		maxSec := int64(maxTimeout / time.Second)
		if *args.Timeout <= 0 || *args.Timeout > maxSec {
			err = fmt.Errorf("timeout must be 1..%d seconds", maxSec)
		} else {
			timeout = time.Duration(*args.Timeout) * time.Second
		}
	}
	if err == nil && args.MaxOutput != nil {
		if *args.MaxOutput < 0 || *args.MaxOutput > int64(math.MaxInt) {
			err = fmt.Errorf("max_output must be >= 0 bytes (0 = no truncation)")
		} else {
			limit = int(*args.MaxOutput)
		}
	}
	if err == nil {
		var result shell.Result
		if result, err = a.Shell.ExecuteLimit(ctx, args.Command, timeout, limit); err == nil {
			return result
		}
	}
	return shell.Result{Output: err.Error(), ExitCode: 2, Error: true}
}

// itemID identifies a response item of the latest request in display events.
func (a *Agent) itemID(index int64) string {
	return fmt.Sprintf("%s-%d", a.lastRequest.RequestID, index)
}

func (a *Agent) request(ctx context.Context, emit func(Event), checkpoints ...func(int64, json.RawMessage) error) (items []json.RawMessage, err error) {
	defer func() { err = a.safeError(err) }()
	if strings.TrimSpace(a.settings.Model) == "" || strings.TrimSpace(a.settings.Provider) == "" {
		return nil, fmt.Errorf("no provider or model selected: use /login to add a provider, then choose a model with /settings")
	}
	prompt := a.instructions()
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
			Description: openai.String(bashToolDescription),
			Parameters:  a.bashParameters(),
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

			// Some providers deliver complete message.content without text deltas.
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

			for _, record := range RecordsForItem(json.RawMessage(event.Item.RawJSON())) {
				emit(Event{Kind: record.Kind, Text: record.Text, ID: a.itemID(event.OutputIndex), Final: true, Provisional: event.Type == "response.output_item.added"})
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
				a.lastRequest.Usage = &Usage{Input: event.Response.Usage.InputTokens, Output: event.Response.Usage.OutputTokens, Cached: cached}
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

func (a *Agent) sessionID() string {
	return strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl")
}

func (a *Agent) SessionID() string {
	return a.sessionID()
}

func (a *Agent) Start(version string) {
	a.version = version
}

func (a *Agent) Finish(code int) {}
