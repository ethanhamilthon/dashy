package agent

import (
	"encoding/json"
	"strings"
	"time"
)

// Usage belongs to one request, never to the whole session.
type Usage struct {
	Input  int64
	Output int64
	Cached int64 // -1 means the provider omitted cached-token data.
}

type Metadata struct {
	Duration     time.Duration
	Usage        *Usage
	ContextInput *int64
	RequestID    string
	At           time.Time `json:",omitzero"` // when an answer arrived
}

type Record struct {
	Preview bool
	Kind    string
	Text    string
	ID      string
	Meta    *Metadata
}

type Event struct {
	Kind        string
	Text        string
	ID          string
	Meta        *Metadata
	Final       bool
	Provisional bool
}

// ApplyEvent applies one display event to a list of display records.
func ApplyEvent(records []Record, e Event) []Record {
	kind := e.Kind
	switch kind {
	case "delta", "refusal_delta":
		kind = "assistant"
	case "reasoning_delta":
		kind = "reasoning"
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

// RecordsForItem is the only Responses-to-display conversion.
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
		if IsNotice(text) {
			kind = "notice"
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
			return []Record{{Kind: "output", Text: string(item)}}
		}
	}
	return nil
}

// Records returns the display records derived from the session history. Answers
// keep the metadata (time, duration, usage) they were shown with.
func (a *Agent) Records() []Record {
	var records []Record
	for _, item := range a.History {
		meta := a.itemMeta[string(item)]
		for _, r := range RecordsForItem(item) {
			if r.Kind == "assistant" && meta != nil {
				m := *meta
				r.Meta = &m
			}
			records = append(records, r)
		}
	}
	return records
}
