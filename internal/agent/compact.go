package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/responses"
	"github.com/openai/openai-go/v3/shared"
)

const (
	tokenBytes   = 4    // rough bytes per token for mixed prose, code and JSON
	imageTokens  = 1200 // charged per image so base64 payloads cannot dominate
	summaryLimit = 1200
)

const summaryInstruction = "Summarize this conversation so work can continue. " +
	"Keep decisions, file paths, commands, results and open questions. Do not use tools."

var dataURLPayload = regexp.MustCompile(`data:image/[^"]*`)

// estimateTokens approximates the prompt size of the whole history.
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

// userTurn reports whether item starts a new conversational turn.
func userTurn(item json.RawMessage) bool {
	var v struct{ Role, Type string }
	if json.Unmarshal(item, &v) != nil || v.Role != "user" || v.Type == "function_call_output" {
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

// summarize condenses old items into one summary text.
func (a *Agent) summarize(ctx context.Context, old []json.RawMessage, prompt string) (string, error) {
	var text strings.Builder
	for _, item := range old {
		for _, r := range RecordsForItem(item) {
			fmt.Fprintf(&text, "[%s] %s\n", r.Kind, r.Text)
		}
	}
	instruction := summaryInstruction
	if prompt != "" {
		instruction += " Instruction: " + prompt
	}
	system, err := a.instructions(ctx)
	if err != nil {
		return "", err
	}
	request := responses.ResponseNewParams{
		Model:           a.settings.Model,
		Instructions:    openai.String(system),
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
// Callers guarantee an idle session.
func (a *Agent) Compact(ctx context.Context, prompt string) (string, error) {
	cut := turnStart(a.History, CompactKeepTurns)
	// Compacting an existing summary would only replace it with another summary.
	if cut == 0 || (cut == 1 && strings.Contains(string(a.History[0]), "compact_summary")) {
		return "Nothing to compact.", nil
	}
	old := a.History[:cut]
	summary, err := a.summarize(ctx, old, strings.TrimSpace(prompt))
	if err != nil {
		return "", a.safeError(err)
	}
	item := raw(map[string]any{"role": "user", "content": "<compact_summary>\n" + summary + "\n</compact_summary>"})
	history, records := a.History, a.records
	a.defaultTitle = a.DefaultTitle() // kept before the first user message can be dropped
	a.History = append([]json.RawMessage{item}, a.History[cut:]...)
	a.records = recordsOf(a.History)
	if err := a.Save(); err != nil {
		a.History, a.records = history, records
		return "", err
	}
	a.lastRequest = Metadata{} // the next size is estimated from the new history
	return fmt.Sprintf("Compacted %d items into a summary; kept the last %d turns.", len(old), CompactKeepTurns), nil
}

// compactIfNeeded summarizes older turns before a request would exceed the token budget.
// A successful compaction emits a reset so the display is rebuilt from the new history.
func (a *Agent) compactIfNeeded(ctx context.Context, emit func(Event)) {
	if !a.settings.AutoCompact || estimateTokens(a.History) <= CompactThreshold {
		return
	}
	result, err := a.Compact(ctx, "")
	if err != nil {
		emit(Event{Kind: "error", Text: "Compaction failed: " + err.Error()})
		return
	}
	emit(Event{Kind: "reset"})
	emit(Event{Kind: "notice", Text: result})
	if estimateTokens(a.History) > CompactThreshold {
		emit(Event{Kind: "notice", Text: "Compaction could not reach threshold; older turns have been summarized."})
	}
}
