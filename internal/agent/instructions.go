package agent

import "encoding/json"

func instructionSnapshot(item json.RawMessage) (string, bool) {
	var v struct{ Type, Text string }
	_ = json.Unmarshal(item, &v)
	return v.Text, v.Type == "instructions"
}

func (a *Agent) requestInput() []json.RawMessage {
	input := make([]json.RawMessage, 0, len(a.History))
	for _, item := range a.History {
		if _, snapshot := instructionSnapshot(item); !snapshot {
			input = append(input, item)
		}
	}
	return input
}
