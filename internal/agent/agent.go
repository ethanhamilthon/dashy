package agent

import (
	"cmp"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"dashy/internal/shell"
)

var BashToolDescription = strings.TrimSpace(bashMD)

//go:embed bash.md
var bashMD string

var codeField = regexp.MustCompile(`"command"\s*:\s*"`)

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

type Options struct {
	Config                  string
	Resume                  string
	Mode                    string
	Provider, Model, Effort string
	Observe                 func(ExecutionEvent)
}

type Agent struct {
	opts         Options
	home         string
	cfg          *settingsState
	Shell        *shell.Runner
	History      []json.RawMessage
	itemMeta     map[int]*Metadata
	db           *sql.DB
	project      string
	id           string
	saved        int
	stored       bool
	owner        *os.File
	promptFiles  []string
	prompt       string
	promptFrozen bool
	promptSource string
	title        string
	lastRequest  Metadata
	sock         *sessionSocket
	queue        *Steering
	slots        *imageSlots
	tasks        *tasks
}

func New(dir string, opts Options) (*Agent, error) {
	home, err := DashyHome(dir)
	if err != nil {
		return nil, err
	}
	if opts.Mode == "" {
		opts.Mode = "tui"
	}
	a := &Agent{opts: opts, home: home, promptSource: "startup", Shell: &shell.Runner{Dir: dir}, slots: &imageSlots{}, queue: &Steering{}}
	a.Shell.Env = a.shellEnv
	if a.db, err = openDB(home); err != nil {
		return nil, err
	}
	if err := a.loadConfig(dir); err != nil {
		a.db.Close()
		return nil, err
	}
	_ = CleanAttachments(a.AttachmentsDir())
	if err := a.openProject(dir); err != nil {
		a.Close()
		return nil, err
	}
	return a, nil
}

func (a *Agent) loadConfig(dir string) error {
	if a.cfg == nil {
		a.cfg = &settingsState{}
	}
	base, version, err := loadSettings(a.db)
	if err != nil {
		return err
	}
	files, err := instructionFiles(a.home, dir, a.opts)
	if err != nil {
		return err
	}
	a.cfg.mu.Lock()
	err = a.applyLocked(base, version)
	a.cfg.mu.Unlock()
	if err != nil {
		return err
	}
	a.promptFiles = files
	return nil
}

func raw(v any) json.RawMessage { data, _ := json.Marshal(v); return data }

func (a *Agent) message(role, text string) {
	a.History = append(a.History, raw(map[string]any{"role": role, "content": text}))
}

func (a *Agent) saveHistory() error {
	if err := a.Save(); err != nil {
		return fmt.Errorf("could not save history: %w", err)
	}
	return nil
}

func (a *Agent) Run(ctx context.Context, text string, emit func(Event)) error {
	return a.RunInput(ctx, Input{Text: text}, emit)
}

func (a *Agent) RunInput(ctx context.Context, input Input, emit func(Event)) (err error) {
	defer func() {
		err = a.safeError(err)
		if err != nil && !errors.Is(err, context.Canceled) {
			emit(Event{Kind: "error", Text: err.Error()})
		}
		if a.HasSavedSession() {
			if e := a.saveHistory(); e != nil {
				err = errors.Join(err, e)
				emit(Event{Kind: "error", Text: e.Error()})
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
		accepted := false
		for _, instruction := range a.queue.Take() {
			if strings.TrimSpace(instruction.Text) != "" || len(instruction.Images) > 0 {
				a.addInput(instruction, emit)
				accepted = true
			}
		}
		return accepted
	}

	shown := inputEvent(input)
	shown.ID = fmt.Sprintf("input-%d", time.Now().UnixNano())
	a.commitInput(input)
	shown.Meta = a.itemMeta[len(a.History)-1]
	emit(shown)

	turnSeq := len(a.History) - 1
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		acceptSteering()
		a.refreshSettings(emit)
		if e := a.saveHistory(); e != nil {
			return e
		}
		items, e := a.requestWithRetries(ctx, emit)
		if e != nil {
			return e
		}
		meta := a.lastRequest
		meta.RequestSeq = len(a.History) + 1
		meta.TurnSeq = turnSeq
		meta.At = time.Now()
		calls := 0
		for i, item := range items {
			a.History = append(a.History, item)
			a.setItemMeta(len(a.History)-1, meta)
			for _, r := range RecordsForItem(item) {
				m := meta
				if r.Kind == "assistant" {
					m = a.answerMetadata(len(a.History) - 1)
				}
				emit(Event{Kind: r.Kind, Text: r.Text, ID: a.itemID(int64(i)), Meta: &m, Final: true})
			}
		}
		if e := a.saveHistory(); e != nil {
			return e
		}
		for _, item := range items {
			call, e := parseCall(item)
			if e != nil {
				return e
			}
			if call.Type != "function_call" {
				continue
			}
			calls++
			started := time.Now()
			result := a.runTool(ctx, call.CallID, call.Name, call.Arguments, emit)
			output := callOutput(call.CallID, result)
			a.History = append(a.History, output)
			m := Metadata{Duration: time.Since(started), At: time.Now()}
			a.setItemMeta(len(a.History)-1, m)
			for _, r := range RecordsForItem(output) {
				emit(Event{Kind: r.Kind, Text: r.Text, Meta: &m, Final: true})
			}
			if e := a.saveHistory(); e != nil {
				return e
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if steered := acceptSteering(); calls == 0 && !steered {
			return nil
		}
	}
}

type functionCall struct {
	Type, Name, Arguments string
	CallID                string `json:"call_id"`
}

func parseCall(item json.RawMessage) (call functionCall, err error) {
	err = json.Unmarshal(item, &call)
	return call, err
}

func callOutput(callID string, result shell.Result) json.RawMessage {
	return raw(map[string]any{"type": "function_call_output", "call_id": callID, "output": string(raw(result))})
}

func (a *Agent) runTool(ctx context.Context, callID, name, arguments string, emit func(Event)) (result shell.Result) {
	var args struct {
		Command string `json:"command"`
		Timeout *int64 `json:"timeout"`
	}
	err := json.Unmarshal([]byte(arguments), &args)
	execution := Execution{ID: fmt.Sprintf("%d:%s", len(a.History), callID), Kind: "bash", Command: args.Command, Started: time.Now()}
	a.observe(execution, "pending", shell.Result{})
	defer func() { a.completed(execution, result) }()
	if ctx.Err() != nil {
		return shell.Result{Output: ctx.Err().Error(), ExitCode: 130, Error: true}
	}
	if a.queue.hasTyped() {
		return shell.Result{Output: "Skipped: the user sent a new message before this call started. Read it first; run the command again if it is still needed."}
	}
	if name != "bash" {
		return shell.Result{Output: "Unknown tool: " + name, Error: true}
	}
	timeout := time.Duration(ShellTimeout) * time.Second
	limit := a.Settings().ShellOutput
	if err == nil && args.Timeout != nil {
		if *args.Timeout <= 0 || *args.Timeout > ShellMaxTimeout {
			err = fmt.Errorf("timeout must be 1..%d seconds", ShellMaxTimeout)
		} else {
			timeout = time.Duration(*args.Timeout) * time.Second
		}
	}
	if err != nil {
		return shell.Result{Output: err.Error(), ExitCode: 2, Error: true}
	}
	a.observe(execution, "in_progress", shell.Result{})
	var detach <-chan struct{}
	if a.tasks != nil {
		a.queue.drainTyped()
		detach = a.queue.typedSignal()
	}
	result, bg := a.Shell.Run(ctx, args.Command, timeout, limit, detach)
	if bg == nil {
		return result
	}
	id, err := a.tasks.adopt(args.Command, bg)
	if err != nil {
		_ = bg.Cmd.Cancel()
		_ = bg.Wait()
		return shell.Result{Output: result.Output + "\n[dashy: stopped; could not move it to the background: " + err.Error() + "]\n", ExitCode: 130, Error: true}
	}
	emit(Event{Kind: "notice", Text: "Command moved to the background as task " + id + "."})
	return shell.Result{Output: fmt.Sprintf("The user sent a new message, so this command moved to the background as task %s and keeps running. "+
		"Its result arrives as <task-result id=%q> when it ends; check it with: dashy tasks status %s\n\nOutput so far:\n%s", id, id, id, cmp.Or(result.Output, "(none)"))}
}

func (a *Agent) itemID(index int64) string {
	return fmt.Sprintf("%s-%d", a.lastRequest.RequestID, index)
}

func (a *Agent) request(ctx context.Context, emit func(Event)) (items []json.RawMessage, err error) {
	defer func() { err = a.safeError(err) }()
	a.refreshSettings(emit)
	settings, client := a.snapshot()
	if hint := settings.SetupHint(); hint != "" {
		return nil, errors.New(hint)
	}
	prompt, err := a.instructions(ctx)
	if err != nil {
		return nil, err
	}
	for i, item := range a.History {
		if !json.Valid(item) {
			return nil, fmt.Errorf("invalid history item %d", i)
		}
	}
	payload := map[string]any{
		"model":        settings.Model,
		"instructions": prompt,
		"input":        a.requestInput(),
		"store":        false,
		"tools": []any{map[string]any{
			"type": "function", "name": "bash", "description": BashToolDescription,
			"parameters": bashParameters(), "strict": true,
		}},
	}
	reasoning := map[string]any{}
	if settings.Effort != "" {
		reasoning["effort"] = settings.Effort
	}
	if settings.ReasoningSummary {
		reasoning["summary"] = "auto"
	}
	if len(reasoning) > 0 {
		payload["reasoning"] = reasoning
	}
	started := time.Now()
	a.lastRequest = Metadata{RequestID: fmt.Sprintf("r-%d", len(a.History))}
	a.slots.sent()
	var completed []json.RawMessage
	arguments := map[int64]string{}
	names := map[int64]string{}
	err = client.stream(ctx, payload, func(event streamEvent) error {
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
			var item struct{ Type, Name, Arguments string }
			_ = json.Unmarshal(event.Item, &item)
			// Some providers deliver complete arguments without argument deltas.
			if item.Type == "function_call" {
				names[event.OutputIndex] = item.Name
				if item.Arguments != "" {
					arguments[event.OutputIndex] = item.Arguments
				}
				if item.Name == "bash" {
					emit(Event{Kind: "code", Text: partialCode(arguments[event.OutputIndex]), Provisional: true, ID: a.itemID(event.OutputIndex), Final: true})
					return nil
				}
			}
			if event.Type == "response.output_item.added" {
				for _, record := range RecordsForItem(event.Item) {
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
			if u := event.Response.Usage; u != nil {
				n := u.InputTokens
				a.lastRequest.ContextInput = &n
				cached := int64(-1)
				if u.InputTokensDetails != nil && u.InputTokensDetails.CachedTokens != nil {
					cached = *u.InputTokensDetails.CachedTokens
				}
				a.lastRequest.Usage = &Usage{Input: n, Output: u.OutputTokens, Cached: cached}
			}
			completed = append([]json.RawMessage{}, event.Response.Output...)
			return errStreamDone
		case "response.failed", "response.incomplete", "error":
			return streamFailure(event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if completed == nil {
		return nil, errIncompleteStream
	}
	return completed, nil
}

const (
	tokenBytes  = 4
	imageTokens = 1200
)

var dataURLPayload = regexp.MustCompile(`data:image/[^"]*`)

func estimateTokens(history []json.RawMessage) int64 {
	var total int64
	for _, item := range history {
		var v struct{ Content []struct{ Type string } }
		if json.Unmarshal(item, &v) == nil {
			for _, part := range v.Content {
				if part.Type == "input_image" {
					total += imageTokens
				}
			}
		}
		total += int64(len(dataURLPayload.ReplaceAllString(string(item), "data:image"))) / tokenBytes
	}
	return total
}

func (a *Agent) ContextTokens() int64 {
	if a.lastRequest.ContextInput != nil {
		return *a.lastRequest.ContextInput
	}
	return estimateTokens(a.requestInput()) + int64(len(a.prompt))/tokenBytes
}

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

func (a *Agent) PromptFiles() []string { return append([]string(nil), a.promptFiles...) }

func (a *Agent) SessionID() string { return a.id }

func bashParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"command": map[string]any{"type": "string"},
			"timeout": map[string]any{
				"type":        []string{"integer", "null"},
				"description": fmt.Sprintf("seconds, default %d, maximum %d", ShellTimeout, ShellMaxTimeout),
			},
		},
		"required":             []string{"command", "timeout"},
		"additionalProperties": false,
	}
}

func (a *Agent) setItemMeta(seq int, m Metadata) {
	if a.itemMeta == nil {
		a.itemMeta = map[int]*Metadata{}
	}
	a.itemMeta[seq] = &m
}
