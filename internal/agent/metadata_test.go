package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func usageReply(output string, input, out int, cached string) string {
	return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":%s,"usage":{"input_tokens":%d,"output_tokens":%d%s}}}`, output, input, out, cached))
}

func TestPerRequestMetadataAndResume(t *testing.T) {
	for _, reportCache := range []bool{false, true} {
		t.Run(fmt.Sprint(reportCache), func(t *testing.T) {
			cache := ""
			if reportCache {
				cache = `,"input_tokens_details":{"cached_tokens":50}`
			}
			answer := `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"same"}]}`
			call := `{"type":"function_call","name":"bash","call_id":"c","arguments":"{\"command\":\"true\"}"}`
			fp := &fakeProvider{reply: func(n int, _ string) string {
				if n == 1 {
					return usageReply("["+answer+","+call+"]", 100, 10, cache)
				}
				return usageReply("["+answer+"]", 200, 20, cache)
			}}
			a := newTestAgent(t, fp, Options{})
			var live []Record
			if err := a.Run(context.Background(), "go", func(e Event) { live = ApplyEvent(live, e) }); err != nil {
				t.Fatal(err)
			}
			records := a.Records()
			if len(records) != 5 {
				t.Fatalf("records %+v", records)
			}
			for i, r := range records {
				if r.Meta == nil {
					t.Fatalf("record %d missing metadata", i)
				}
				if r.Kind == "user" && r.Meta.At.IsZero() {
					t.Fatal("missing user time")
				}
				if r.Kind == "output" && r.Meta.Duration <= 0 {
					t.Fatal("missing tool duration")
				}
				if string(raw(r.Meta)) != string(raw(live[i].Meta)) {
					t.Fatalf("live/resume %d: %s != %s", i, raw(r.Meta), raw(live[i].Meta))
				}
			}
			if records[1].Meta.Usage.Input != 100 || records[4].Meta.Usage.Input != 300 || records[4].Meta.Usage.Output != 30 {
				t.Fatalf("totals %+v %+v", records[1].Meta.Usage, records[4].Meta.Usage)
			}
			wantCache := int64(-1)
			if reportCache {
				wantCache = 100
			}
			if records[4].Meta.Usage.Cached != wantCache {
				t.Fatal(records[4].Meta.Usage)
			}
			var own []int64
			for seq, item := range a.History {
				for _, r := range RecordsForItem(item) {
					if r.Kind == "assistant" {
						own = append(own, a.itemMeta[seq].Usage.Input)
					}
				}
			}
			if fmt.Sprint(own) != "[100 200]" {
				t.Fatal(own)
			}
			var stored string
			if err := a.db.QueryRow(`SELECT meta FROM items WHERE session=? AND meta LIKE '%RequestSeq%' LIMIT 1`, a.id).Scan(&stored); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(stored, "RequestID") {
				t.Fatal(stored)
			}
			id, dir := a.id, a.Shell.Dir
			a.Close()
			again, err := New(dir, Options{Resume: id})
			if err != nil {
				t.Fatal(err)
			}
			defer again.Close()
			if string(raw(again.Records())) != string(raw(records)) {
				t.Fatalf("metadata changed on resume\n%s\n%s", raw(records), raw(again.Records()))
			}
		})
	}
}

func TestUsageSurvivesTurnWithoutAnswer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fp := &fakeProvider{reply: func(int, string) string {
		return usageReply(`[{"type":"function_call","name":"bash","call_id":"c","arguments":"{\"command\":\"true\"}"}]`, 42, 7, "")
	}}
	a := newTestAgent(t, fp, Options{})
	_ = a.Run(ctx, "go", func(e Event) {
		if e.Kind == "output" && e.Final {
			cancel()
		}
	})
	for seq, item := range a.History {
		var call struct{ Type string }
		_ = json.Unmarshal(item, &call)
		if call.Type == "function_call" {
			m := a.itemMeta[seq]
			if m == nil || m.Usage == nil || m.Usage.Input != 42 || m.Usage.Cached != -1 {
				t.Fatalf("call metadata %+v", m)
			}
			return
		}
	}
	t.Fatal("call not committed")
}

func TestUserMetadataUsesQueueTime(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	before := time.Now()
	a.Queue().Push(Input{Text: "queued"})
	after := time.Now()
	input := a.Queue().Take()[0]
	a.commitInput(input)
	at := a.Records()[0].Meta.At
	if at.Before(before) || at.After(after) {
		t.Fatalf("message time %s outside enqueue interval", at)
	}
}
