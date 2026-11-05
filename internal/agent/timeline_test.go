package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRecordsDerivedFromHistory(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "say hi"); err != nil {
		t.Fatal(err)
	}
	records := a.Records()
	var kinds []string
	for _, r := range records {
		kinds = append(kinds, r.Kind)
	}
	if strings.Join(kinds, ",") != "user,code,output,assistant" {
		t.Fatalf("records %+v", records)
	}
}

func TestReasoningSummaryIsKept(t *testing.T) {
	item := json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"weighed the options"}]}`)
	records := RecordsForItem(item)
	if len(records) != 1 || records[0].Kind != "reasoning" || records[0].Text != "weighed the options" {
		t.Fatalf("records %+v", records)
	}
}
