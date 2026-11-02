package agent

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"dashy/internal/shell"
)

// The resume list title is the custom title, else the first line of the first
// user message; rename and rename-reset both go through the saved meta line.
func TestSessionTitlesAndRenameReset(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "  first line\nsecond line"); err != nil {
		t.Fatal(err)
	}
	list := func() string {
		sessions, err := a.Sessions()
		if err != nil || len(sessions) != 1 {
			t.Fatalf("sessions: %v %v", sessions, err)
		}
		return sessions[0].Title
	}
	if got := list(); got != "first line" {
		t.Fatalf("default title = %q", got)
	}
	if err := a.Rename("  Named   work "); err != nil {
		t.Fatal(err)
	}
	if got := list(); got != "Named work" || a.CustomTitle() != "Named work" {
		t.Fatalf("renamed title = %q", got)
	}
	if err := a.Rename(""); err != nil {
		t.Fatal(err)
	}
	if got := list(); got != "first line" || a.CustomTitle() != "" {
		t.Fatalf("reset title = %q", got)
	}
}

// A resumed session keeps its custom title and its history, and its context size
// is estimated because no provider usage is known yet.
func TestResumeKeepsTitleHistoryAndEstimatesContext(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	turn(t, a, "hello there")
	a.Rename("Kept")
	id := a.SessionID()
	if _, err := New(a.Shell.Dir, Options{Resume: id}); err == nil {
		t.Fatal("a session in use by this process was opened twice")
	}
	a.Close()
	again, err := New(a.Shell.Dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.CustomTitle() != "Kept" || len(again.History) != 2 || again.ContextTokens() <= 0 {
		t.Fatalf("resumed: title %q, %d items, ctx %d", again.CustomTitle(), len(again.History), again.ContextTokens())
	}
}

// A torn last line (a crash mid-write) is dropped; earlier lines are kept.
func TestTornLastLineIsDropped(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	a.message("user", "kept")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(a.HistoryPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"role":"user","content":"tor`)
	f.Close()
	if err := a.readSession(); err != nil || len(a.History) != 1 {
		t.Fatalf("read after torn line: %d items, %v", len(a.History), err)
	}
}

func TestSessionLockIsExclusive(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	_, err := New(a.Shell.Dir, Options{Resume: a.SessionID()})
	if err == nil || !strings.Contains(err.Error(), "in use by another dashy process") {
		t.Fatalf("second owner: %v", err)
	}
}

// The socket accepts only PNG or JPEG payloads that decode as their own format.
func TestValidImage(t *testing.T) {
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG())
	cases := map[string]bool{
		png: true,
		"data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString([]byte("<svg/>")): false,
		"data:image/png;base64,!!!": false,
		"data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(tinyPNG()): false,
		"https://example.com/a.png": false,
	}
	for url, want := range cases {
		if got := validImage(shell.Image{URL: url}); got != want {
			t.Errorf("validImage(%.40s...) = %v", url, got)
		}
	}
}

// tinyPNG is a valid 1x1 PNG.
func tinyPNG() []byte {
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==")
	return data
}

// A session file holds history items, one per line, plus one meta line for the title.
func TestSessionFileHoldsHistoryItemsAndTitleOnly(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := a.Rename("Plan"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	metas := 0
	for _, line := range lines {
		var v map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("line is not an object: %s", line)
		}
		if _, ok := v["meta"]; ok {
			metas++
		} else if _, ok := v["role"]; !ok {
			t.Fatalf("history line without a role: %s", line)
		}
	}
	if len(lines) != 3 || metas != 1 {
		t.Fatalf("lines %d, meta lines %d:\n%s", len(lines), metas, data)
	}
}
