package acp

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"dashy/internal/agent"

	sdk "github.com/coder/acp-go-sdk"
)

func configResult(t *testing.T, f frame) []configOption {
	t.Helper()
	if f.Error != nil {
		t.Fatal(f.Error)
	}
	var result struct {
		ConfigOptions []configOption `json:"configOptions"`
	}
	if err := json.Unmarshal(f.Result, &result); err != nil {
		t.Fatal(err)
	}
	var contract struct {
		ConfigOptions []sdk.SessionConfigOption `json:"configOptions"`
	}
	if err := json.Unmarshal(f.Result, &contract); err != nil {
		t.Fatalf("SDK rejected options: %v", err)
	}
	if len(result.ConfigOptions) != 2 || len(contract.ConfigOptions) != 2 {
		t.Fatalf("missing selectors: %s", f.Result)
	}
	return result.ConfigOptions
}
func setConfig(t *testing.T, w *wire, id int, session, key, value string) []configOption {
	t.Helper()
	w.request(t, id, "session/set_config_option", map[string]string{"sessionId": session, "configId": key, "value": value})
	return configResult(t, w.response(t, id))
}

func TestSessionModelAndEffortConfiguration(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	home := settings(t, func(body string) string { mu.Lock(); bodies = append(bodies, body); mu.Unlock(); return answer("ok") })
	w := connect(t)
	w.initialize(t)
	dir := t.TempDir()
	w.request(t, 2, "session/new", map[string]any{"cwd": dir, "mcpServers": []any{}})
	created := w.response(t, 2)
	options := configResult(t, created)
	var result struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.Unmarshal(created.Result, &result)
	a := result.SessionID
	if options[0].Category != "model" || options[0].CurrentValue != "p/m" || len(options[0].Options) != 2 || options[1].Category != "thought_level" || len(options[1].Options) != len(agent.Efforts) {
		t.Fatalf("bad selectors: %+v", options)
	}
	b := w.newSession(t, 3, t.TempDir())
	options = setConfig(t, w, 4, a, "model", "p/m2")
	if options[0].CurrentValue != "p/m2" {
		t.Fatal(options)
	}
	options = setConfig(t, w, 5, a, "effort", "high")
	if options[1].CurrentValue != "high" {
		t.Fatal(options)
	}
	w.prompt(t, 6, a, "first")
	w.response(t, 6)
	w.prompt(t, 7, b, "second")
	w.response(t, 7)
	mu.Lock()
	if len(bodies) != 2 || !strings.Contains(bodies[0], `"model":"m2"`) || !strings.Contains(bodies[0], `"effort":"high"`) || !strings.Contains(bodies[1], `"model":"m"`) || strings.Contains(bodies[1], `"effort":"high"`) {
		t.Fatalf("selection leaked or was ignored: %v", bodies)
	}
	mu.Unlock()
	defaults, err := agent.LoadSettings(home)
	if err != nil || defaults.Model != "m" || defaults.Effort == "high" {
		t.Fatalf("global defaults changed: %+v %v", defaults, err)
	}
	for _, update := range w.updates {
		var p struct {
			SessionID string `json:"sessionId"`
			Update    struct{ SessionUpdate string }
		}
		_ = json.Unmarshal(update, &p)
		if p.Update.SessionUpdate == "config_option_update" && p.SessionID != a {
			t.Fatal("config notification leaked")
		}
	}
	setConfig(t, w, 8, a, "effort", "default")
	w.conn.Close()
	<-w.done
	resumed, err := agent.New(dir, agent.Options{Resume: a})
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	if s := resumed.Settings(); s.Model != "m2" || s.Effort != "" {
		t.Fatalf("selection not persisted: %+v", s)
	}
}

func TestInvalidAndBusyConfigurationIsRejected(t *testing.T) {
	settings(t, func(string) string { return command("sleep 60") })
	w := connect(t)
	w.initialize(t)
	session := w.newSession(t, 2, t.TempDir())
	for _, p := range []map[string]any{
		{"sessionId": session, "configId": "model", "value": "unknown/model"},
		{"sessionId": session, "configId": "effort", "value": "invalid"},
		{"sessionId": session, "configId": "effort", "value": ""},
		{"sessionId": session, "configId": "effort", "value": true},
		{"sessionId": session, "configId": "effort"},
		{"sessionId": session, "configId": "unknown", "value": "high"},
	} {
		w.request(t, 3, "session/set_config_option", p)
		if f := w.response(t, 3); f.Error == nil || f.Error.Code != -32602 {
			t.Fatal(f)
		}
	}
	w.prompt(t, 4, session, "work")
	for {
		f := w.next(t)
		if f.Method == "session/update" && strings.Contains(string(f.Params), "in_progress") {
			break
		}
	}
	w.request(t, 5, "session/set_config_option", map[string]string{"sessionId": session, "configId": "effort", "value": "high"})
	if f := w.response(t, 5); f.Error == nil || f.Error.Code != -32600 {
		t.Fatal(f)
	}
	w.cancel(t, session)
	w.response(t, 4)
}

func TestModelIDsDoNotCollideAcrossProviderNames(t *testing.T) {
	if modelID("a/b", "c") == modelID("a", "b/c") {
		t.Fatal("provider/model ambiguity")
	}
}
