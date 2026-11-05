package agent

import (
	"encoding/json"

	"dashy/internal/shell"
)

func interruptedOutputs(history []json.RawMessage) []json.RawMessage {
	var calls []string
	outputs := map[string]bool{}
	for _, item := range history {
		v, err := parseCall(item)
		if err != nil || v.CallID == "" {
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
		result = append(result, callOutput(id, shell.Result{Output: "Tool execution was interrupted; its effects and result are unknown. It was not rerun.", Error: true}))
	}
	return result
}
