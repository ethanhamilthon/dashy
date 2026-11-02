package agent

import (
	"encoding/json"
	"testing"
)

// The live display records of a turn match the records derived from the saved history,
// so a resumed session shows exactly what the live session showed.
func TestLiveRecordsMatchRecordsDerivedFromHistory(t *testing.T) {
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
	live, derived := a.Records(), recordsOf(a.History)
	if len(live) != 4 || len(live) != len(derived) {
		t.Fatalf("live %+v, derived %+v", live, derived)
	}
	for i := range live {
		if live[i].Kind != derived[i].Kind || live[i].Text != derived[i].Text {
			t.Fatalf("record %d: live %+v, derived %+v", i, live[i], derived[i])
		}
	}
}

// Reasoning items keep their summary text, which is what the display shows.
func TestReasoningSummaryIsKept(t *testing.T) {
	item := json.RawMessage(`{"type":"reasoning","summary":[{"type":"summary_text","text":"weighed the options"}]}`)
	records := RecordsForItem(item)
	if len(records) != 1 || records[0].Kind != "reasoning" || records[0].Text != "weighed the options" {
		t.Fatalf("records %+v", records)
	}
}
