package agent

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"dashy/internal/hooks"
)

// failingBoot is a mandatory session_start hook that prints the secret and fails.
func failingBoot(a *Agent) {
	a.hooks.Set([]hooks.Hook{{Name: "boot", Events: []string{"session_start"}, Command: `echo "leak ` + secretKey + `"; exit 3`, SendOutput: true}})
}

func TestStartupGateWithdrawsInputWithoutRequest(t *testing.T) {
	requests := 0
	a, closeAll := secretAgent(t, false, func(w http.ResponseWriter, r *http.Request) {
		requests++
		complete(w)
	})
	defer closeAll()
	failingBoot(a)
	a.Start("test")
	var events []Event
	err := a.RunInput(context.Background(), Input{Text: "keep me"}, func(e Event) { events = append(events, e) })
	if err == nil || requests != 0 {
		t.Fatalf("err = %v, requests = %d; a failed startup must block the request", err, requests)
	}
	withdrawn := false
	texts := []string{err.Error()}
	for _, e := range events {
		texts = append(texts, e.Text)
		withdrawn = withdrawn || (e.Kind == "withdraw" && e.Text == "keep me" && e.ID == "")
	}
	if !withdrawn || len(a.History) != 0 {
		t.Fatalf("input not withdrawn intact: events %+v, history %d", events, len(a.History))
	}
	assertNoSecret(t, a, texts...)
}

func TestManualCompactBlockedByStartupGate(t *testing.T) {
	summaries := 0
	a, closeAll := secretAgent(t, false, func(w http.ResponseWriter, r *http.Request) {
		summaries++
		writeSummary(w, "condensed")
	})
	defer closeAll()
	failingBoot(a)
	a.Start("test")
	a.History = compactHistory(t, 5)
	a.settings.CompactKeepTurns = 1
	before := append(a.History[:0:0], a.History...)
	_, err := a.Compact(context.Background(), "")
	if err == nil || summaries != 0 {
		t.Fatalf("err = %v, summary requests = %d; a failed startup must block compaction", err, summaries)
	}
	if len(a.History) != len(before) || string(a.History[0]) != string(before[0]) {
		t.Fatal("blocked compaction changed the history")
	}
	assertNoSecret(t, a, err.Error())
}

// A startup hook that failed because of the environment succeeds after the
// environment is fixed and reloaded: Reload must apply settings before retrying.
func TestReloadRetriesStartupWithNewEnvironment(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	var bodies [][]byte
	server := fakeModel(&bodies)
	defer server.Close()
	writeConfig := func(env string) {
		write(t, filepath.Join(home, "dashy.toml"), fmt.Sprintf(`provider = 'test'
model = 'm'
[providers.test]
base_url = '%s'
[hooks.boot]
events = ['session_start']
send_output = true
command = 'if [ -z "$STARTUP_CTX_TEST" ]; then echo "context missing"; exit 3; fi; echo "ctx=$STARTUP_CTX_TEST"'
%s`, server.URL, env))
	}
	writeConfig("")
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.Start("test")
	if err := a.RunInput(context.Background(), Input{Text: "first"}, func(Event) {}); err == nil {
		t.Fatal("startup failure must block the turn")
	}
	if len(bodies) != 0 {
		t.Fatal("a request was sent despite the failed startup")
	}
	writeConfig("[env]\nSTARTUP_CTX_TEST = 'ready-ctx'\n")
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := a.RunInput(context.Background(), Input{Text: "second"}, func(Event) {}); err != nil {
		t.Fatalf("first input after the fix failed: %v", err)
	}
	if len(bodies) != 1 || !strings.Contains(string(bodies[0]), "ctx=ready-ctx") {
		t.Fatalf("retried startup did not use the new environment: %d requests", len(bodies))
	}
}
