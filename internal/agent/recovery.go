package agent

import (
	"encoding/json"

	"dashy/internal/shell"
)

// interruptedOutputs closes incomplete call/output pairs without repeating effects.
func interruptedOutputs(history []json.RawMessage) []json.RawMessage {
	var calls []string
	outputs := map[string]bool{}
	for _, item := range history {
		var v struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
		}
		if json.Unmarshal(item, &v) != nil || v.CallID == "" {
			continue
		}
		if v.Type == "function_call" {
			calls = append(calls, v.CallID)
		}
		if v.Type == "function_call_output" {
			outputs[v.CallID] = true
		}
	}
	var result []json.RawMessage
	for _, id := range calls {
		if outputs[id] {
			continue
		}
		outputs[id] = true
		value := shell.Result{Output: "Tool execution was interrupted; its effects and result are unknown. It was not rerun.", Error: true}
		result = append(result, raw(map[string]any{"type": "function_call_output", "call_id": id, "output": string(raw(value))}))
	}
	return result
}
