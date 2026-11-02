package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// Usage belongs to one request, never to the whole session.
type Usage struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Cached int64 `json:"cached"` // -1 means the provider omitted cached-token data.
}

type Metadata struct {
	Duration     time.Duration `json:"duration"`
	Usage        *Usage        `json:"usage,omitempty"`
	ContextInput *int64        `json:"context_input,omitempty"`
	RequestID    string        `json:"request_id,omitempty"`
}

type Record struct {
	Preview bool      `json:"preview,omitempty"`
	Kind    string    `json:"kind"`
	Text    string    `json:"text,omitempty"`
	ID      string    `json:"id,omitempty"`
	Meta    *Metadata `json:"meta,omitempty"`
}

type Event struct {
	Kind        string    `json:"kind"`
	Text        string    `json:"text,omitempty"`
	ID          string    `json:"id,omitempty"`
	Meta        *Metadata `json:"meta,omitempty"`
	Final       bool      `json:"final,omitempty"`
	Steering    bool      `json:"steering,omitempty"`
	Provisional bool      `json:"provisional,omitempty"`
}

// ApplyEvent is shared by live display and the persisted display journal.
func ApplyEvent(records []Record, e Event) []Record {
	kind := e.Kind
	switch kind {
	case "delta":
		kind = "assistant"
	case "reasoning_delta":
		kind = "reasoning"
	case "refusal_delta":
		kind = "assistant"
	}
	if e.ID != "" {
		for i := len(records) - 1; i >= 0; i-- {
			if records[i].ID == e.ID {
				if e.Final {
					records[i].Text = e.Text
				} else {
					records[i].Text += e.Text
				}
				records[i].Kind = kind
				records[i].Preview = e.Provisional
				if e.Meta != nil {
					records[i].Meta = e.Meta
				}
				return records
			}
		}
	} else if e.Kind == "delta" && len(records) > 0 && records[len(records)-1].Kind == kind {
		records[len(records)-1].Text += e.Text
		return records
	}
	return append(records, Record{Kind: kind, Text: e.Text, ID: e.ID, Meta: e.Meta, Preview: e.Provisional})
}

// RecordsForItem is the only Responses-to-display conversion, including old histories.
func RecordsForItem(item json.RawMessage) []Record {
	var v struct {
		Role, Type, Name, Arguments, Output string
		Content                             json.RawMessage
		Summary                             []struct{ Text string }
	}
	if json.Unmarshal(item, &v) != nil {
		return nil
	}
	switch {
	case v.Role == "user" || v.Role == "assistant" || v.Type == "message":
		var text string
		if json.Unmarshal(v.Content, &text) != nil {
			var parts []struct {
				Type    string `json:"type"`
				Text    string `json:"text"`
				Refusal string `json:"refusal"`
			}
			if json.Unmarshal(v.Content, &parts) == nil {
				var texts []string
				for _, p := range parts {
					if p.Text != "" {
						texts = append(texts, p.Text)
					}
					if p.Refusal != "" {
						texts = append(texts, p.Refusal)
					}
				}
				text = strings.Join(texts, "\n")
			}
		}
		kind := v.Role
		if kind == "" {
			kind = "assistant"
		}
		if strings.HasPrefix(text, "<notify>") {
			kind = "notice"
		} else if strings.HasPrefix(text, "<compact_summary>") {
			summary := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(text, "<compact_summary>"), "</compact_summary>"))
			if summary != "" {
				return []Record{{Kind: "notice", Text: "Compacted conversation: " + summary}}
			}
			return []Record{{Kind: "notice", Text: "Compacted earlier conversation."}}
		}
		if text != "" {
			return []Record{{Kind: kind, Text: text}}
		}
	case v.Type == "reasoning":
		var texts []string
		for _, p := range v.Summary {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) == 0 {
			var s struct {
				Summary string `json:"summary"`
			}
			if json.Unmarshal(item, &s) == nil && s.Summary != "" {
				texts = append(texts, s.Summary)
			}
		}
		var parts []struct{ Text string }
		_ = json.Unmarshal(v.Content, &parts)
		for _, p := range parts {
			if p.Text != "" {
				texts = append(texts, p.Text)
			}
		}
		if len(texts) == 0 {
			var c string
			if json.Unmarshal(v.Content, &c) == nil && c != "" {
				texts = append(texts, c)
			}
		}
		if len(texts) > 0 {
			return []Record{{Kind: "reasoning", Text: strings.Join(texts, "\n")}}
		}
	case v.Type == "function_call":
		var args struct{ Command string }
		_ = json.Unmarshal([]byte(v.Arguments), &args)
		if v.Name == "bash" {
			return []Record{{Kind: "code", Text: args.Command}}
		}
		return []Record{{Kind: "code", Text: v.Name + "\n" + v.Arguments}}
	case v.Type == "function_call_output":
		var result struct {
			Output string
			Error  bool
		}
		if json.Unmarshal([]byte(v.Output), &result) == nil {
			kind := "output"
			if result.Error {
				kind = "error"
			}
			return []Record{{Kind: kind, Text: result.Output}}
		}
		return []Record{{Kind: "output", Text: v.Output}}
	default:
		// Provider-managed tool events remain visible even without a specialized UI.
		if v.Type != "" {
			return []Record{{Kind: "notice", Text: string(item)}}
		}
	}
	return nil
}

// Records returns the display records of the session: the live snapshot while
// a turn or a display change keeps one, else the records derived from history.
func (a *Agent) Records() []Record {
	if a.records != nil {
		return append([]Record(nil), a.records...)
	}
	return recordsOf(a.History)
}

func recordsOf(history []json.RawMessage) []Record {
	var records []Record
	for _, item := range history {
		records = append(records, RecordsForItem(item)...)
	}
	return records
}

// RememberDisplay retains UI records for the live session in memory.
func (a *Agent) RememberDisplay(records []Record) {
	a.records = make([]Record, 0, len(records))
	for _, record := range records {
		if !record.Preview {
			a.records = append(a.records, record)
		}
	}
}
