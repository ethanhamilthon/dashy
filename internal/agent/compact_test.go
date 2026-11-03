package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// summaryReply is a non-streamed response with one message, as a summary request expects.
func summaryReply(text string) string {
	return fmt.Sprintf(`{"id":"s","object":"response","status":"completed","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}]}`, text)
}

// Manual compaction keeps the last turns, replaces older items by one summary and
// passes the guidance prompt to the summary request.
func TestCompactKeepsLastTurnsAndSummarizesTheRest(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return summaryReply("SUMMARY") }}
	a := newTestAgent(t, fp, Options{})
	compactTurns(t, a, 6, 10)
	text, err := a.Compact(context.Background(), "focus on tests")
	if err != nil || text != "Compacted 4 items into a summary; kept the last 4 turns." {
		t.Fatalf("compact: %q %v", text, err)
	}
	if len(a.History) != 9 || !strings.Contains(string(a.History[0]), "SUMMARY") || !strings.Contains(string(a.History[1]), "turn 2") {
		t.Fatalf("history after compaction: %d items, first %s", len(a.History), a.History[0])
	}
	if !strings.Contains(fp.requests()[0], "Instruction: focus on tests") {
		t.Fatal("guidance prompt not sent to the summary request")
	}
}

// Nothing is compacted when only the kept turns exist, and a summary is not summarized again.
func TestCompactNothingToDoAndSummaryIsNotResummarized(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return summaryReply("SUMMARY") }}
	a := newTestAgent(t, fp, Options{})
	compactTurns(t, a, 4, 10)
	if text, err := a.Compact(context.Background(), ""); err != nil || text != "Nothing to compact." {
		t.Fatalf("small history: %q %v", text, err)
	}
	compactTurns(t, a, 2, 10)
	if _, err := a.Compact(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if text, _ := a.Compact(context.Background(), ""); text != "Nothing to compact." || len(fp.requests()) != 1 {
		t.Fatalf("second compaction: %q after %d requests", text, len(fp.requests()))
	}
}

// A failed summary leaves the history exactly as it was.
func TestCompactFailureKeepsHistory(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return "not json" }}
	a := newTestAgent(t, fp, Options{})
	compactTurns(t, a, 6, 10)
	before := len(a.History)
	if _, err := a.Compact(context.Background(), ""); err == nil || len(a.History) != before {
		t.Fatalf("failed compaction: err %v, items %d -> %d", err, before, len(a.History))
	}
}

// Automatic compaction runs before a request that exceeds the threshold, and the
// display is rebuilt from the compacted history: old turns disappear, the summary appears.
func TestAutomaticCompactionShrinksHistoryAndReports(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if strings.Contains(body, `"stream":true`) {
			return textReply("after")
		}
		return summaryReply("SUMMARY")
	}}
	a := newTestAgent(t, fp, Options{})
	compactTurns(t, a, 6, 300000)
	events, err := turn(t, a, "next")
	if err != nil {
		t.Fatal(err)
	}
	var reported bool
	for _, e := range events {
		reported = reported || e.Kind == "notice" && strings.HasPrefix(e.Text, "Compacted")
	}
	joined := fmt.Sprint(a.Records())
	if !reported || !strings.Contains(joined, "Compacted conversation: SUMMARY") || strings.Contains(joined, "turn 0") {
		t.Fatalf("reported %v, records %s", reported, joined)
	}
}
