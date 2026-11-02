package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const (
	// tokenBytes is a rough bytes-per-token ratio for mixed prose, code and JSON.
	tokenBytes   = 4
	imageTokens  = 1200 // charged per image so base64 payloads cannot dominate
	summaryLimit = 1200
)

const summaryInstruction = "Summarize this conversation so work can continue. " +
	"Keep decisions, file paths, commands, results and open questions. Do not use tools."

// stripDataURLs drops inline base64 payloads so an image is charged a fixed cost
// instead of megabytes of encoded pixels.
func stripDataURLs(text string) string {
	var out strings.Builder
	for {
		start := strings.Index(text, `"data:`)
		if start < 0 {
			break
		}
		end := strings.IndexByte(text[start+6:], '"')
		if end < 0 {
			break
		}
		out.WriteString(text[:start+1])
		out.WriteString("data:image")
		text = text[start+6+end:]
	}
	out.WriteString(text)
	return out.String()
}

// estimateTokens approximates the prompt size of the whole history.
func estimateTokens(history []json.RawMessage) int64 {
	var total int64
	for _, item := range history {
		text := stripDataURLs(string(item))
		var v struct{ Content []struct{ Type string } }
		if json.Unmarshal(item, &v) == nil {
			for _, part := range v.Content {
				if part.Type == "input_image" {
					total += imageTokens
				}
			}
		}
		total += int64(len(text)) / tokenBytes
	}
	return total
}

// userTurn reports whether item starts a new conversational turn.
func userTurn(item json.RawMessage) bool {
	var v struct{ Role, Type string }
	if json.Unmarshal(item, &v) != nil {
		return false
	}
	if v.Role != "user" || v.Type == "function_call_output" {
		return false
	}
	records := RecordsForItem(item)
	return len(records) == 1 && records[0].Kind == "user" && !strings.HasPrefix(records[0].Text, "<compact_summary>")
}

// turnStart returns the index where the last n user turns begin, or 0 when there are fewer.
func turnStart(history []json.RawMessage, turns int) int {
	found := 0
	for i := len(history) - 1; i >= 0; i-- {
		if userTurn(history[i]) {
			found++
			if found == turns {
				return i
			}
		}
	}
	return 0
}

// instructions is the system prompt used by every request.
func (a *Agent) instructions() string {
	return a.System
}

// summarize condenses the old part into one summary written under the session instructions.
func (a *Agent) summarize(ctx context.Context, old []json.RawMessage, prompt string) (string, error) {
	var text strings.Builder
	for _, item := range old {
		for _, r := range RecordsForItem(item) {
			fmt.Fprintf(&text, "[%s] %s\n", r.Kind, r.Text)
		}
	}
	instruction := summaryInstruction
	if prompt = strings.TrimSpace(prompt); prompt != "" {
		instruction += " Instruction: " + prompt
	}
	request := responses.ResponseNewParams{
		Model:           a.settings.Model,
		Instructions:    openai.String(a.instructions()),
		Store:           openai.Bool(false),
		MaxOutputTokens: openai.Int(summaryLimit),
		Input: responses.ResponseNewParamsInputUnion{OfInputItemList: responses.ResponseInputParam{
			{OfMessage: &responses.EasyInputMessageParam{
				Role:    responses.EasyInputMessageRoleUser,
				Content: responses.EasyInputMessageContentUnionParam{OfString: openai.String(instruction + "\n\n" + text.String())},
			}},
		}},
	}
	if a.settings.Effort != "" {
		request.Reasoning.Effort = shared.ReasoningEffort(a.settings.Effort)
	}
	response, err := a.client.Responses.New(ctx, request)
	if err != nil {
		return "", err
	}
	if response.Status != responses.ResponseStatusCompleted {
		return "", fmt.Errorf("summary did not complete: %s", response.Status)
	}
	var summary strings.Builder
	for _, item := range response.Output {
		for _, part := range item.Content {
			if part.Type == "output_text" {
				summary.WriteString(part.Text)
			}
		}
	}
	if strings.TrimSpace(summary.String()) == "" {
		return "", fmt.Errorf("provider returned an empty summary")
	}
	return strings.TrimSpace(summary.String()), nil
}

// Compact replaces everything before the last CompactKeepTurns turns with one summary.
// Callers ensure an idle session.
func (a *Agent) Compact(ctx context.Context, prompt string) (string, error) {
	return a.compact(ctx, prompt, "manual")
}

func (a *Agent) compact(ctx context.Context, prompt, reason string) (text string, err error) {
	keep := max(1, a.settings.CompactKeepTurns)
	cut := turnStart(a.History, keep)
	// Compacting an existing summary would replace it with another summary.
	// json.Marshal escapes "<", so the marker is matched without the brackets.
	if cut == 0 || cut == 1 && strings.Contains(string(a.History[0]), "compact_summary") {
		return "Nothing to compact.", nil
	}
	old := a.History[:cut]
	defer func() {
		err = a.safeError(err)
	}()
	summary, err := a.summarize(ctx, old, prompt)
	if err != nil {
		return "", err
	}
	item := raw(map[string]any{"role": "user", "content": "<compact_summary>\n" + summary + "\n</compact_summary>"})
	// Compaction shortens only the model context. The display journal keeps every
	// earlier record, so it is never rebuilt from the summary.
	if a.records == nil {
		a.records = a.Records()
	}
	history, records := a.History, a.records
	a.History = append([]json.RawMessage{item}, a.History[cut:]...)
	if err := a.Save(); err != nil {
		a.History, a.records = history, records
		return "", err
	}
	return fmt.Sprintf("Compacted %d items into a summary; kept the last %d turns.", len(old), keep), nil
}

// compactIfNeeded summarizes older turns before a request would exceed the token budget.
func (a *Agent) compactIfNeeded(ctx context.Context, emit func(Event)) {
	limit := a.settings.MaxContextTokens
	if limit <= 0 || estimateTokens(a.History) <= limit {
		return
	}
	result, err := a.compact(ctx, "", "auto")
	if err != nil {
		emit(Event{Kind: "error", Text: "Compaction failed: " + err.Error()})
		return
	}
	emit(Event{Kind: "notice", Text: result})
	if estimateTokens(a.History) > limit {
		emit(Event{Kind: "notice", Text: "Compaction could not reach max_context_tokens; raise it to leave room for the kept turns."})
	}
}
