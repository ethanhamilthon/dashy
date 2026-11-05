package acp

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/coder/acp-go-sdk"
)

func TestResumeRestoresWithoutReplay(t *testing.T) {
	requests := make(chan string, 8)
	settings(t, func(body string) string { requests <- body; return answer("noted") })
	dir := t.TempDir()
	w := connect(t)
	w.initialize(t)
	id := w.newSession(t, 2, dir)
	w.prompt(t, 3, id, "first message")
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	<-requests
	w.conn.Close()
	<-w.done

	w2 := connect(t)
	w2.initialize(t)
	w2.request(t, 2, "session/resume", map[string]any{"sessionId": id, "cwd": dir})
	f := w2.response(t, 2)
	if f.Error != nil {
		t.Fatal(f.Error)
	}
	var response sdk.ResumeSessionResponse
	if err := json.Unmarshal(f.Result, &response); err != nil || len(response.ConfigOptions) == 0 {
		t.Fatalf("resume response: %s %v", f.Result, err)
	}
	for _, raw := range w2.updates {
		if !strings.Contains(string(raw), "usage_update") {
			t.Fatalf("resume replayed history: %s", raw)
		}
	}
	w2.prompt(t, 3, id, "second message")
	if f := w2.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	if body := <-requests; !strings.Contains(body, "first message") || !strings.Contains(body, "noted") {
		t.Fatalf("resume lost history: %s", body)
	}
}

func TestListSessionsFiltersAndPages(t *testing.T) {
	settings(t, func(string) string { return answer("ok") })
	a, b := t.TempDir(), t.TempDir()
	w := connect(t)
	w.initialize(t)
	ids := map[string]string{}
	for i := range listPage + 2 {
		dir := a
		if i == 0 {
			dir = b
		}
		id := w.newSession(t, 100+2*i, dir)
		w.prompt(t, 101+2*i, id, "title "+id)
		if f := w.response(t, 101+2*i); f.Error != nil {
			t.Fatal(f.Error)
		}
		ids[id] = dir
		w.request(t, 1000+i, "session/close", map[string]string{"sessionId": id})
		if f := w.response(t, 1000+i); f.Error != nil {
			t.Fatal(f.Error)
		}
	}
	seen := map[string]bool{}
	var cursor *string
	pages := 0
	for {
		params := map[string]any{"cwd": a}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		w.request(t, 3, "session/list", params)
		f := w.response(t, 3)
		var page sdk.ListSessionsResponse
		if f.Error != nil || json.Unmarshal(f.Result, &page) != nil {
			t.Fatalf("list: %s %v", f.Result, f.Error)
		}
		pages++
		for _, info := range page.Sessions {
			if seen[string(info.SessionId)] || ids[string(info.SessionId)] != a || info.Cwd != a || info.Title == nil || !strings.HasPrefix(*info.Title, "title ") || info.UpdatedAt == nil {
				t.Fatalf("bad listed session: %+v", info)
			}
			seen[string(info.SessionId)] = true
		}
		if cursor = page.NextCursor; cursor == nil {
			break
		}
	}
	if pages != 2 || len(seen) != listPage+1 {
		t.Fatalf("listed %d sessions in %d pages", len(seen), pages)
	}
	w.request(t, 4, "session/list", map[string]any{})
	var all sdk.ListSessionsResponse
	if f := w.response(t, 4); f.Error != nil || json.Unmarshal(f.Result, &all) != nil || len(all.Sessions) != listPage {
		t.Fatalf("unfiltered list: %s", f.Result)
	}
	for _, params := range []map[string]any{{"cwd": "relative"}, {"cursor": "nonsense"}} {
		w.request(t, 5, "session/list", params)
		if f := w.response(t, 5); f.Error == nil || f.Error.Code != -32602 {
			t.Fatal(f)
		}
	}
}

func TestCloseCancelsTurnAndReleasesSession(t *testing.T) {
	settings(t, func(body string) string {
		if strings.Contains(body, "function_call_output") {
			return answer("done")
		}
		return command("sleep 30")
	})
	dir := t.TempDir()
	w := connect(t)
	w.initialize(t)
	id := w.newSession(t, 2, dir)
	w.prompt(t, 3, id, "run something long")
	for !strings.Contains(string(w.next(t).Params), `"tool_call"`) {
	}
	w.request(t, 4, "session/close", map[string]string{"sessionId": id})
	var prompt, closed frame
	for prompt.ID == nil || closed.ID == nil {
		switch f := w.next(t); string(f.ID) {
		case "3":
			prompt = f
		case "4":
			closed = f
		}
	}
	if prompt.Error != nil || !strings.Contains(string(prompt.Result), "cancelled") || closed.Error != nil {
		t.Fatalf("close: prompt %s %v, close %s %v", prompt.Result, prompt.Error, closed.Result, closed.Error)
	}
	w.prompt(t, 5, id, "still there?")
	if f := w.response(t, 5); f.Error == nil || f.Error.Code != -32002 {
		t.Fatal(f)
	}
	w.request(t, 6, "session/close", map[string]string{"sessionId": id})
	if f := w.response(t, 6); f.Error == nil || f.Error.Code != -32002 {
		t.Fatal(f)
	}
	if f := load(t, w, 7, id, filepath.Clean(dir)); f.Error != nil {
		t.Fatal(f.Error)
	}
}
