package acp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"dashy/internal/agent"
)

func TestProviderErrorsAreSafeAndNonempty(t *testing.T) {
	for _, body := range []string{
		`{"type":"error"}`,
		`{"type":"response.failed","response":{"error":{}}}`,
		`{"type":"error","error":{"code":"bad_key","message":"sk-test-secret"}}`,
	} {
		t.Run(body, func(t *testing.T) {
			settings(t, func(string) string { return "data: " + body + "\n\n" })
			w := connect(t)
			w.initialize(t)
			session := w.newSession(t, 2, t.TempDir())
			w.prompt(t, 3, session, "hi")
			f := w.response(t, 3)
			if f.Error == nil || f.Error.Code != -32603 || len(f.Error.Message) < 10 || strings.Contains(f.Error.Message, "sk-test-secret") {
				t.Fatalf("unsafe/empty error: %+v", f)
			}
		})
	}
}

func TestHTTPAuthFailureIsAuthenticationRequired(t *testing.T) {
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"error":{"message":"invalid sk-test-secret"}}`)
	}))
	defer provider.Close()
	config := agent.DefaultSettings()
	config.Provider, config.Model = "p", "m"
	config.Providers["p"] = agent.Provider{BaseURL: provider.URL, APIKey: "sk-test-secret"}
	if err := agent.SaveSettings(home, config); err != nil {
		t.Fatal(err)
	}
	w := connect(t)
	w.initialize(t)
	session := w.newSession(t, 2, t.TempDir())
	w.prompt(t, 3, session, "hi")
	f := w.response(t, 3)
	if f.Error == nil || f.Error.Code != -32000 || strings.Contains(f.Error.Message, "sk-test-secret") {
		t.Fatal(f)
	}
}

func TestBackgroundHelpersSurviveTurnCancelAndStopOnDisconnect(t *testing.T) {
	gate := make(chan struct{})
	started := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(gate) })
	home := settings(t, func(body string) string {
		var p struct{ Input json.RawMessage }
		_ = json.Unmarshal([]byte(body), &p)
		if strings.Contains(string(p.Input), `"slow"`) {
			close(started)
			<-gate
		}
		return answer("ok")
	})
	w := connect(t)
	w.initialize(t)
	dir := t.TempDir()
	session := w.newSession(t, 2, dir)
	w.prompt(t, 3, session, "first")
	w.response(t, 3)
	id, err := agent.RunTask(dir, session, "sleep 60")
	if err != nil || id == "" {
		t.Fatalf("background helper: %s %v", id, err)
	}
	w.prompt(t, 4, session, "slow")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never received prompt")
	}
	w.cancel(t, session)
	if f := w.response(t, 4); !strings.Contains(string(f.Result), "cancelled") {
		t.Fatal(f)
	}
	tasks, err := agent.SessionTasks(home, session)
	if err != nil || len(tasks) != 1 || tasks[0].Status != "running" {
		t.Fatalf("task unexpectedly stopped: %+v %v", tasks, err)
	}
	release.Do(func() { close(gate) })
	w.conn.Close()
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("disconnect hung")
	}
	tasks, err = agent.SessionTasks(home, session)
	if err != nil || tasks[0].Status == "running" {
		t.Fatalf("task leaked after disconnect: %+v %v", tasks, err)
	}
}

func TestIdleTaskResultStartsTurn(t *testing.T) {
	settings(t, func(body string) string {
		if strings.Contains(body, "task-result") {
			return answer("woke")
		}
		return answer("ok")
	})
	w := connect(t)
	w.initialize(t)
	dir := t.TempDir()
	session := w.newSession(t, 2, dir)
	w.prompt(t, 3, session, "first")
	w.response(t, 3)
	if _, err := agent.RunTask(dir, session, "true"); err != nil {
		t.Fatal(err)
	}
	for {
		f := w.next(t)
		if f.Method == "session/update" && strings.Contains(string(f.Params), `"woke"`) {
			break
		}
	}
	w.prompt(t, 4, session, "next")
	if f := w.response(t, 4); f.Error != nil {
		t.Fatal(f.Error)
	}
}
