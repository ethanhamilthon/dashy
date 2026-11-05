package agent

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// editFile simulates another dashy process changing the stored settings while the agent works.
func editFile(t *testing.T, a *Agent, change func(*Settings)) {
	t.Helper()
	s, err := LoadSettings(a.home)
	if err != nil {
		t.Fatal(err)
	}
	change(&s)
	if err := SaveSettings(a.home, s); err != nil {
		t.Fatal(err)
	}
}

// storeRaw stores settings data as a new version without validating it.
func storeRaw(t *testing.T, a *Agent, data string) {
	t.Helper()
	if _, err := a.db.Exec(`UPDATE settings SET data = ?, version = version + 1 WHERE id = 1`, data); err != nil {
		t.Error(err)
	}
}

// reloadAgent makes a provider whose first reply runs a tool call after hook has run: the
// edit happens while the turn is in flight, so the second request is the first to see it.
func reloadAgent(t *testing.T, opts Options, hook func(a *Agent)) (*Agent, *fakeProvider) {
	t.Helper()
	var a *Agent
	fp := &fakeProvider{}
	fp.reply = func(n int, _ string) string {
		if n == 1 {
			hook(a)
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}
	a = newTestAgent(t, fp, opts)
	return a, fp
}

// Settings saved by another process during a turn do not stop it; the next request uses
// the new shared values but keeps this session's model and effort.
func TestFileEditDuringTurnAppliesToNextRequest(t *testing.T) {
	a, fp := reloadAgent(t, Options{}, func(a *Agent) {
		editFile(t, a, func(s *Settings) { s.Model, s.Effort, s.ReasoningSummary = "new-model", "high", false })
	})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[0], `"model":"m"`) || strings.Contains(reqs[0], `"effort"`) {
		t.Fatalf("first request: %v", reqs)
	}
	if !strings.Contains(reqs[1], `"model":"m"`) || strings.Contains(reqs[1], `"effort"`) || strings.Contains(reqs[1], `"summary"`) {
		t.Fatalf("second request: %s", reqs[1])
	}
	// A new session starts with the new default.
	if err := a.NewSession(); err != nil || a.Settings().Model != "new-model" {
		t.Fatalf("new session model %q, %v", a.Settings().Model, err)
	}
}

// A change made through the agent (the TUI path) during a turn is used by the next request too.
func TestChangeDuringTurnAppliesToNextRequest(t *testing.T) {
	a, fp := reloadAgent(t, Options{}, func(a *Agent) {
		if err := a.Select("p", "picked", "low"); err != nil {
			t.Error(err)
		}
	})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	if reqs := fp.requests(); !strings.Contains(reqs[1], `"model":"picked"`) || !strings.Contains(reqs[1], `"effort":"low"`) {
		t.Fatalf("second request: %s", reqs[1])
	}
}

// Broken stored JSON keeps the old settings, warns once, and a later fix is picked up.
func TestBrokenSettingsKeepOldAndWarnOnce(t *testing.T) {
	a, fp := reloadAgent(t, Options{}, func(a *Agent) {
		storeRaw(t, a, `{"model": `)
	})
	events, err := turn(t, a, "go")
	if err != nil {
		t.Fatal(err)
	}
	if reqs := fp.requests(); len(reqs) != 2 || !strings.Contains(reqs[1], `"model":"m"`) {
		t.Fatalf("requests: %v", reqs)
	}
	warnings := 0
	for _, e := range events {
		if e.Kind == "notice" && strings.Contains(e.Text, "keeping the previous settings") {
			warnings++
		}
	}
	if warnings != 1 {
		t.Fatalf("%d warnings, want 1: %+v", warnings, events)
	}
	if a.Settings().Model != "m" {
		t.Fatalf("settings changed: %+v", a.Settings())
	}
	storeRaw(t, a, `{"provider":"p","model":"fixed","show_lines":"full","providers":`+providersJSON(a)+`}`)
	a.refreshSettings(nil)
	if a.Settings().ShowLines != "full" {
		t.Fatalf("fixed settings not picked up: %+v", a.Settings())
	}
}

func providersJSON(a *Agent) string {
	return `{"p":{"base_url":"` + a.Settings().Providers["p"].BaseURL + `"}}`
}

// Invalid but well-formed settings (unknown show_lines) are treated like broken JSON.
func TestInvalidSettingsKeepOld(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	storeRaw(t, a, `{"show_lines":"bogus"}`)
	a.refreshSettings(nil)
	if s := a.Settings(); s.Model != "m" || s.ShowLines == "bogus" {
		t.Fatalf("settings: %+v", s)
	}
}

// -p overrides win over the stored settings before and after a reload.
func TestOverridesSurviveReload(t *testing.T) {
	a, fp := reloadAgent(t, Options{Model: "cli-model", Effort: "low"}, func(a *Agent) {
		editFile(t, a, func(s *Settings) { s.Model, s.Effort, s.ReasoningSummary = "file-model", "high", false })
	})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	for i, body := range fp.requests() {
		if !strings.Contains(body, `"model":"cli-model"`) || !strings.Contains(body, `"effort":"low"`) {
			t.Fatalf("request %d lost the overrides: %s", i+1, body)
		}
	}
	if a.Settings().ReasoningSummary {
		t.Fatal("other settings from the file were not reloaded")
	}
}

// The system prompt is built once per session: editing DASHY.md or settings mid-turn does not change it.
func TestPromptStaysFrozenWhileSettingsReload(t *testing.T) {
	var a *Agent
	fp := &fakeProvider{}
	fp.reply = func(n int, _ string) string {
		if n > 1 {
			return textReply("done")
		}
		writeFile(t, filepath.Join(a.Shell.Dir, "DASHY.md"), "changed")
		editFile(t, a, func(s *Settings) { s.ReasoningSummary = false })
		return bashReply("c1", "echo hi")
	}
	a = newTestAgentWith(t, fp, Options{}, func(_, dir string) { writeFile(t, filepath.Join(dir, "DASHY.md"), "original") })
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
	reqs := fp.requests()
	if !strings.Contains(reqs[0], "original") || !strings.Contains(reqs[1], "original") || strings.Contains(reqs[1], "changed") || strings.Contains(reqs[1], `"summary"`) {
		t.Fatalf("prompt changed within the session:\n%s\n%s", reqs[0], reqs[1])
	}
}

// Changes from other goroutines (the TUI) and file edits race with a running turn; run with -race.
func TestConcurrentSettingsChangesDuringTurn(t *testing.T) {
	a, _ := reloadAgent(t, Options{}, func(a *Agent) {
		var wg sync.WaitGroup
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					_ = a.ChangeSettings(func(s *Settings) { s.ReasoningSummary = !s.ReasoningSummary })
					_ = a.Settings()
					a.refreshSettings(nil)
				}
			}()
		}
		wg.Wait()
	})
	if _, err := turn(t, a, "go"); err != nil {
		t.Fatal(err)
	}
}
