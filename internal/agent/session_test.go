package agent

import (
	"encoding/base64"
	"strings"
	"testing"

	"dashy/internal/shell"
)

// The resume list title is the custom title, else the first line of the first
// user message; rename and rename-reset are both saved.
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
	if again.CustomTitle() != "Kept" || len(again.History) != 3 || again.ContextTokens() <= 0 {
		t.Fatalf("resumed: title %q, %d items, ctx %d", again.CustomTitle(), len(again.History), again.ContextTokens())
	}
}

// A rename is separate from the default title (the first user line), and its
// reset restores that default in the list and after reopening.
func TestRenameResetRestoresDefaultTitle(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	a.message("user", "turn 0 x")
	a.message("assistant", "answer 0")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	const want = "turn 0 x"
	if a.DefaultTitle() != want {
		t.Fatalf("default %q", a.DefaultTitle())
	}
	if err := a.Rename("Mine"); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := a.Sessions(); sessions[0].Title != "Mine" {
		t.Fatalf("renamed list title = %q", sessions[0].Title)
	}
	if err := a.Rename(""); err != nil {
		t.Fatal(err)
	}
	if sessions, _ := a.Sessions(); sessions[0].Title != want {
		t.Fatalf("reset list title = %q", sessions[0].Title)
	}
	id := a.SessionID()
	a.Close()
	again, err := New(a.Shell.Dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.DefaultTitle() != want || again.CustomTitle() != "" {
		t.Fatalf("reopened: default %q, custom %q", again.DefaultTitle(), again.CustomTitle())
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

// storedItems reads the history items stored for the session.
func storedItems(t *testing.T, a *Agent) []string {
	t.Helper()
	rows, err := a.db.Query(`SELECT data FROM items WHERE session = ? ORDER BY seq`, a.SessionID())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var items []string
	for rows.Next() {
		var item string
		if err := rows.Scan(&item); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	return items
}

// The database holds every history item once, in order, and the title in the session row.
func TestSessionRowsHoldHistoryItemsAndTitle(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	if err := a.Rename("Plan"); err != nil {
		t.Fatal(err)
	}
	items := storedItems(t, a)
	if len(items) != 3 || len(items) != len(a.History) || items[0] != string(a.History[0]) {
		t.Fatalf("stored items %v", items)
	}
	var title string
	if err := a.db.QueryRow(`SELECT title FROM sessions WHERE id = ?`, a.SessionID()).Scan(&title); err != nil || title != "Plan" {
		t.Fatalf("title %q, %v", title, err)
	}
}

// A user message whose stored line is over 4 MiB still has its first line as the
// title, in the resume list and after reopening.
func TestLargeFirstMessageKeepsListTitle(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "Short title\n"+strings.Repeat("x", 5<<20)); err != nil {
		t.Fatal(err)
	}
	sessions, err := a.Sessions()
	if err != nil || len(sessions) != 1 || sessions[0].Title != "Short title" {
		t.Fatalf("listed: %v %v", sessions, err)
	}
	id := a.SessionID()
	a.Close()
	again, err := New(a.Shell.Dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if again.DefaultTitle() != "Short title" {
		t.Fatalf("reopened title = %q", again.DefaultTitle())
	}
}

// The session list holds only this project's sessions, newest first, titled by the
// custom title or else the first user line.
func TestSessionListIsScopedAndOrdered(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	for _, row := range [][]any{
		{"a", a.project, "Custom", "first", 3},
		{"b", a.project, "", "Saved first line", 2},
		{"c", "/elsewhere", "", "Other project", 1},
	} {
		if _, err := a.db.Exec(`INSERT INTO sessions (id, project, title, first, updated) VALUES (?, ?, ?, ?, ?)`, row...); err != nil {
			t.Fatal(err)
		}
	}
	sessions, err := a.Sessions()
	if err != nil || len(sessions) != 2 || sessions[0].ID != "a" || sessions[0].Title != "Custom" || sessions[1].Title != "Saved first line" {
		t.Fatalf("sessions %+v, %v", sessions, err)
	}
	if err := a.openSession("c"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("another project's session opened: %v", err)
	}
}

// A model chosen in one session is the default for new sessions but does not change
// another open session, which keeps its model after reopening too.
func TestModelSelectionIsPerSession(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err != nil {
		t.Fatal(err)
	}
	b, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.Select("p", "other", "high"); err != nil {
		t.Fatal(err)
	}
	a.refreshSettings(nil)
	if s := a.Settings(); s.Model != "m" || s.Effort != "" {
		t.Fatalf("open session changed to %s/%s", s.Model, s.Effort)
	}
	if err := a.Select("p", "mine", "low"); err != nil {
		t.Fatal(err)
	}
	id := a.SessionID()
	a.Close()
	again, err := New(a.Shell.Dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if s := again.Settings(); s.Model != "mine" || s.Effort != "low" {
		t.Fatalf("reopened session runs %s/%s", s.Model, s.Effort)
	}
	fresh, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if s := fresh.Settings(); s.Model != "mine" {
		t.Fatalf("new session default %s", s.Model)
	}
}

// An answer keeps its time and duration after a resume.
func TestResumedAnswerKeepsItsTime(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgent(t, fp, Options{})
	turn(t, a, "hello")
	before := a.Records()
	id := a.SessionID()
	a.Close()
	again, err := New(a.Shell.Dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	records := again.Records()
	if len(records) != 2 || records[1].Kind != "assistant" || records[1].Meta == nil || records[1].Meta.At.IsZero() || records[1].Meta.Duration <= 0 {
		t.Fatalf("records %+v", records)
	}
	if !records[1].Meta.At.Equal(before[1].Meta.At) || records[1].Meta.Duration != before[1].Meta.Duration {
		t.Fatalf("meta changed: %+v -> %+v", before[1].Meta, records[1].Meta)
	}
}
