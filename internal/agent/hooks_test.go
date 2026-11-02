package agent

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/hooks"
	"dashy/internal/shell"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

// configuredHooks returns the hooks a's configuration activates, the same list agent.New hands to the runner.
func configuredHooks(t *testing.T, a *Agent) []hooks.Hook {
	t.Helper()
	c, err := loadConfiguration(a.home, a.Shell.Dir, a.files)
	if err != nil {
		t.Fatal(err)
	}
	return c.hooks
}

// hookNames lists the configured hook names of a in order, comma separated.
func hookNames(t *testing.T, a *Agent) string {
	t.Helper()
	var names []string
	for _, h := range configuredHooks(t, a) {
		names = append(names, h.Name)
	}
	return strings.Join(names, ",")
}

func TestConfigSourcesPriority(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	flagged := filepath.Join(t.TempDir(), "flag.toml")

	open := func(files ConfigFiles) *Agent {
		t.Helper()
		a, err := New(cwd, files)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		return a
	}

	// Implicit global missing: neutral built-in defaults used
	a := open(ConfigFiles{})
	if len(a.Sources) != 1 || a.Sources[0].Origin != "none" {
		t.Fatalf("defaults sources: %+v", a.Sources)
	}

	// Home dashy.toml exists
	write(t, filepath.Join(home, "dashy.toml"), "[hooks.h1]\nevents = ['idle']\ncommand = 'true'\n")
	a = open(ConfigFiles{})
	if len(a.Sources) != 1 || a.Sources[0].Origin != "home" {
		t.Fatalf("home sources: %+v", a.Sources)
	}

	// Project dashy.toml exists
	write(t, filepath.Join(cwd, "dashy.toml"), "[hooks.h2]\nevents = ['turn_start']\ncommand = 'true'\n")
	a = open(ConfigFiles{})
	if len(a.Sources) != 2 || a.Sources[0].Origin != "home" || a.Sources[1].Origin != "project" {
		t.Fatalf("project sources: %+v", a.Sources)
	}

	// Explicit --config
	write(t, flagged, "[hooks.h3]\nevents = ['turn_end']\ncommand = 'true'\n")
	a = open(ConfigFiles{Config: flagged})
	if len(a.Sources) != 2 || a.Sources[0].Origin != "home" || a.Sources[1].Origin != "flag" {
		t.Fatalf("flag sources: %+v", a.Sources)
	}

	// --no-extend skips global
	a = open(ConfigFiles{Config: flagged, NoExtend: true})
	if len(a.Sources) != 1 || a.Sources[0].Origin != "flag" {
		t.Fatalf("no-extend sources: %+v", a.Sources)
	}

	// Headless inherits the global and project hooks, same as the TUI
	a = open(ConfigFiles{Headless: true})
	if got := hookNames(t, a); got != "h1,h2" {
		t.Fatalf("headless implicit hooks = %q, want h1,h2", got)
	}

	// Headless with explicit config has hooks
	a = open(ConfigFiles{Headless: true, Config: flagged})
	if got := hookNames(t, a); got != "h1,h3" {
		t.Fatalf("headless explicit hooks = %q, want h1,h3", got)
	}

	// Missing explicit --config must fail
	if _, err := New(cwd, ConfigFiles{Config: filepath.Join(cwd, "missing.toml")}); err == nil {
		t.Fatal("missing --config file must fail")
	}
}

func answer(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello back\"}]}]}}\n\n")
}

func completionServer(t *testing.T, handler func(w http.ResponseWriter)) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w)
	}))
	t.Cleanup(server.Close)
	return server
}

func newHookAgent(t *testing.T, server *httptest.Server) *Agent {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	configureTestAgent(t, a, Settings{Provider: "test", Model: "test-model", Providers: map[string]Provider{"test": {BaseURL: server.URL}}})
	return a
}

func recorder(t *testing.T, a *Agent, events string, names ...string) func() []string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "events")
	line := "$DASHY_EVENT"
	for _, v := range names {
		line += "|$" + v
	}
	a.hooks.Set([]hooks.Hook{{
		Events:  strings.Split(events, ","),
		Command: fmt.Sprintf(`echo "%s" >> %s`, line, out),
	}})
	return func() []string {
		t.Helper()
		a.hooks.Wait(5 * time.Second)
		data, _ := os.ReadFile(out)
		return strings.Fields(string(data))
	}
}

func joined(get func() []string) string {
	return strings.Join(get(), " ")
}

func TestTurnStatusCancelledAndInputVariables(t *testing.T) {
	a := newHookAgent(t, completionServer(t, answer))
	out := filepath.Join(t.TempDir(), "out")
	a.hooks.Set([]hooks.Hook{{
		Events:  []string{"turn_start", "turn_end", "steer_accepted"},
		Command: `echo "$DASHY_EVENT|$DASHY_TURN_STATUS|$DASHY_TURN_SOURCE|$(cat "$DASHY_MESSAGE_FILE" 2>/dev/null)" >> ` + out,
	}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := a.Run(ctx, "never sent", func(Event) {}); err == nil {
		t.Fatal("expected cancellation")
	}
	steering := &Steering{}
	steering.Push("and also")
	if err := a.Run(context.Background(), "<notify>\ntick\n</notify>", func(Event) {}, steering); err != nil {
		t.Fatal(err)
	}
	a.hooks.Wait(5 * time.Second)
	data, _ := os.ReadFile(out)
	want := "turn_start||user|never sent\nturn_end|cancelled|user|\n" +
		"turn_start||notify|<notify>\ntick\n</notify>\nsteer_accepted|||and also\nturn_end|ok|notify|\n"
	if string(data) != want {
		t.Fatalf("got %q\nwant %q", data, want)
	}
}

func TestErrorAndRetryEvents(t *testing.T) {
	calls := 0
	a := newHookAgent(t, completionServer(t, func(w http.ResponseWriter) {
		if calls++; calls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	got := recorder(t, a, "model_retry,error,turn_end", "DASHY_ATTEMPT", "DASHY_TURN_STATUS")
	if err := a.Run(context.Background(), "hi", func(Event) {}); err == nil {
		t.Fatal("expected failure")
	}
	if want := "model_retry|2| error|| turn_end||error"; joined(got) != want {
		t.Fatalf("got %q", joined(got))
	}
}

func TestInputQueueEvents(t *testing.T) {
	a := newHookAgent(t, completionServer(t, answer))
	out := filepath.Join(t.TempDir(), "out")
	a.hooks.Set([]hooks.Hook{{
		Events:  []string{"notify_received", "image_attached"},
		Command: `echo "$DASHY_EVENT|$DASHY_IMAGE_SOURCE|$(cat "$DASHY_NOTIFY_FILE" 2>/dev/null)" >> ` + out,
	}})
	a.inbox <- "<notify>\nbuild done\n</notify>"
	if got := a.Notifications(); len(got) != 1 {
		t.Fatalf("notifications = %v", got)
	}
	a.views <- shell.Image{URL: "data:image/png;base64,AA=="}
	if got := a.viewed(); len(got) != 1 {
		t.Fatalf("views = %v", got)
	}
	a.hooks.Wait(5 * time.Second)
	data, _ := os.ReadFile(out)
	if want := "notify_received||<notify>\nbuild done\n</notify>\nimage_attached|view|\n"; string(data) != want {
		t.Fatalf("got %q want %q", data, want)
	}
}

func TestHookSendOutputDrainedOnNextTurn(t *testing.T) {
	a := newHookAgent(t, completionServer(t, answer))
	a.hooks.Set([]hooks.Hook{{
		Name:       "post-turn",
		Events:     []string{"turn_end"},
		Command:    `[ "$DASHY_TURN_SOURCE" = "user" ] && echo "build success" || true`,
		SendOutput: true,
	}})

	// First turn runs and triggers turn_end hook
	if err := a.Run(context.Background(), "first", func(Event) {}); err != nil {
		t.Fatal(err)
	}
	a.hooks.Wait(3 * time.Second)

	// Second turn drains the hook output before the user's second message
	var notices []string
	if err := a.Run(context.Background(), "second", func(e Event) {
		if e.Kind == "notice" {
			notices = append(notices, e.Text)
		}
	}); err != nil {
		t.Fatal(err)
	}

	const command = `[ "$DASHY_TURN_SOURCE" = "user" ] && echo "build success" || true`
	found := false
	for _, n := range notices {
		if strings.Contains(n, "build success") && strings.Contains(n, "<hook_output") {
			found = true
			if got := hookCommandAttr(t, n); got != command {
				t.Fatalf("emitted notice command = %q, want %q", got, command)
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected hook output delivered as notice on second turn, got %v", notices)
	}

	saved := false
	for _, item := range a.History {
		var msg struct{ Role, Content string }
		if json.Unmarshal(item, &msg) != nil || msg.Role != "user" || !strings.Contains(msg.Content, "<hook_output") {
			continue
		}
		saved = true
		if got := hookCommandAttr(t, msg.Content); got != command {
			t.Fatalf("saved history command = %q, want %q", got, command)
		}
	}
	if !saved {
		t.Fatal("expected hook output saved as user history")
	}
}

// hookCommandAttr decodes the hook_output command attribute from an XML notice.
func hookCommandAttr(t *testing.T, text string) string {
	t.Helper()
	var doc struct {
		HookOutput struct {
			Command string `xml:"command,attr"`
		} `xml:"hook_output"`
	}
	if err := xml.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("invalid hook output XML: %v\n%s", err, text)
	}
	return doc.HookOutput.Command
}

func TestHeadlessHookInheritanceAndValidation(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	globalCfg := `provider = "p"
model = "m"
[providers.p]
base_url = "https://example.com"

[hooks.global_hook]
events = ["turn_end"]
command = "echo global"

[hooks.partial_override]
events = ["turn_start"]
command = "echo from-global"
timeout = 10
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(globalCfg), 0600); err != nil {
		t.Fatal(err)
	}

	// 1. Implicit headless: global hooks are active, as in the TUI
	aImplicit, err := New(cwd, ConfigFiles{Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer aImplicit.Close()
	if got := hookNames(t, aImplicit); got != "global_hook,partial_override" {
		t.Fatalf("implicit headless hooks = %q, want global_hook,partial_override", got)
	}

	// 2. Explicit headless without --no-extend: inherits global hooks, and partial override inherits command/events
	explicitCfg := `provider = "p"
model = "m"
[providers.p]
base_url = "https://example.com"

[hooks.partial_override]
timeout = 5

[hooks.explicit_hook]
events = ["turn_end"]
command = "echo explicit"
`
	explicitPath := filepath.Join(cwd, "explicit.toml")
	if err := os.WriteFile(explicitPath, []byte(explicitCfg), 0600); err != nil {
		t.Fatal(err)
	}

	aExplicit, err := New(cwd, ConfigFiles{Headless: true, Config: explicitPath})
	if err != nil {
		t.Fatalf("explicit headless with partial override should succeed, got: %v", err)
	}
	defer aExplicit.Close()
	hooksList := configuredHooks(t, aExplicit)
	hookNames := map[string]bool{}
	for _, h := range hooksList {
		hookNames[h.Name] = true
	}
	if !hookNames["global_hook"] || !hookNames["partial_override"] || !hookNames["explicit_hook"] {
		t.Fatalf("expected global_hook, partial_override, and explicit_hook; got: %+v", hookNames)
	}

	// 3. Explicit headless with --no-extend: global hooks are skipped
	noExtendCfg := `provider = "p"
model = "m"
[providers.p]
base_url = "https://example.com"

[hooks.explicit_hook]
events = ["turn_end"]
command = "echo explicit"
`
	noExtendPath := filepath.Join(cwd, "no_extend.toml")
	if err := os.WriteFile(noExtendPath, []byte(noExtendCfg), 0600); err != nil {
		t.Fatal(err)
	}
	aNoExt, err := New(cwd, ConfigFiles{Headless: true, Config: noExtendPath, NoExtend: true})
	if err != nil {
		t.Fatal(err)
	}
	defer aNoExt.Close()
	noExtHooks := configuredHooks(t, aNoExt)
	if len(noExtHooks) != 1 || noExtHooks[0].Name != "explicit_hook" {
		t.Fatalf("expected only explicit_hook with --no-extend, got: %+v", noExtHooks)
	}

	// 4. Missing required fields without global definition fails validateHooks (not discarded)
	missingReqCfg := `provider = "p"
model = "m"
[providers.p]
base_url = "https://example.com"

[hooks.incomplete]
timeout = 5
`
	missingReqPath := filepath.Join(cwd, "incomplete.toml")
	if err := os.WriteFile(missingReqPath, []byte(missingReqCfg), 0600); err != nil {
		t.Fatal(err)
	}
	_, err = New(cwd, ConfigFiles{Headless: true, Config: missingReqPath})
	if err == nil {
		t.Fatal("expected validateHooks error for incomplete hook, but got nil")
	}
}

func TestHeadlessSupportField(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)
	write(t, filepath.Join(home, "dashy.toml"), `provider = "p"
model = "m"
[providers.p]
base_url = "https://example.com"

[hooks.default_on]
events = ["turn_end"]
command = "true"

[hooks.tui_only]
events = ["turn_end"]
command = "true"
headless_support = false
`)
	open := func(files ConfigFiles) *Agent {
		t.Helper()
		a, err := New(cwd, files)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.Close)
		return a
	}

	// Default is true; the TUI runs every hook, including headless_support = false.
	tui := open(ConfigFiles{})
	if got := hookNames(t, tui); got != "default_on,tui_only" {
		t.Fatalf("tui hooks = %q", got)
	}
	for _, h := range configuredHooks(t, tui) {
		if h.HeadlessSupport == nil {
			t.Fatalf("hook %s: headless_support not resolved", h.Name)
		}
		if want := h.Name != "tui_only"; *h.HeadlessSupport != want {
			t.Fatalf("hook %s: headless_support = %v, want %v", h.Name, *h.HeadlessSupport, want)
		}
	}

	// Headless skips the hook with headless_support = false.
	if got := hookNames(t, open(ConfigFiles{Headless: true})); got != "default_on" {
		t.Fatalf("headless hooks = %q, want default_on", got)
	}

	// A project dashy.toml can flip the flag for an inherited hook by name.
	write(t, filepath.Join(cwd, "dashy.toml"), "[hooks.default_on]\nheadless_support = false\n\n[hooks.tui_only]\nheadless_support = true\n")
	if got := hookNames(t, open(ConfigFiles{Headless: true})); got != "tui_only" {
		t.Fatalf("headless hooks with project override = %q, want tui_only", got)
	}
	if got := hookNames(t, open(ConfigFiles{})); got != "default_on,tui_only" {
		t.Fatalf("tui hooks with project override = %q", got)
	}

	// Non-boolean values are rejected by strict parsing.
	bad := filepath.Join(t.TempDir(), "bad.toml")
	write(t, bad, "[hooks.bad]\nevents = [\"turn_end\"]\ncommand = \"true\"\nheadless_support = \"no\"\n")
	if _, err := New(cwd, ConfigFiles{Config: bad}); err == nil {
		t.Fatal("non-boolean headless_support must fail")
	}
}

// prepare waits for the mandatory session_start hook once; ordinary session_start hooks are not awaited.
func TestPrepareGatesOnMandatorySessionStartOnce(t *testing.T) {
	a := newHookAgent(t, completionServer(t, answer))
	count := filepath.Join(t.TempDir(), "count")
	a.hooks.Set([]hooks.Hook{
		{Name: "boot", Events: []string{"session_start"}, Command: "sleep 0.3; echo ready; echo x >> " + count, SendOutput: true},
		{Name: "lazy", Events: []string{"session_start"}, Command: "sleep 30"},
	})
	defer a.hooks.Wait(0)
	a.Start("test")
	started := time.Now()
	if err := a.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("prepare waited %v", elapsed)
	}
	if data, _ := os.ReadFile(count); string(data) != "x\n" {
		t.Fatalf("bootstrap ran more than once: %q", data)
	}
	if out := a.hooks.DrainOutputs(a.sessionID()); len(out) != 1 || !strings.Contains(out[0].Output, "ready") {
		t.Fatalf("outputs = %+v", out)
	}
}

func TestPrepareFailureBlocksUntilReload(t *testing.T) {
	a := newHookAgent(t, completionServer(t, answer))
	boot := func(cmd string) hooks.Hook {
		return hooks.Hook{Name: "boot", Events: []string{"session_start"}, Command: cmd, SendOutput: true}
	}
	a.hooks.Set([]hooks.Hook{boot("echo broken; exit 3")})
	defer a.hooks.Wait(0)
	a.Start("test")
	if err := a.prepare(context.Background()); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
	if err := a.prepare(context.Background()); err == nil {
		t.Fatal("failure did not persist for the session")
	}
	a.hooks.Set([]hooks.Hook{boot("echo fixed")}) // reload retries the bootstrap
	if err := a.prepare(context.Background()); err != nil {
		t.Fatalf("reload did not retry: %v", err)
	}
}

func TestPrepareCancellationKeepsStartupResult(t *testing.T) {
	a := newHookAgent(t, completionServer(t, answer))
	a.hooks.Set([]hooks.Hook{{Name: "boot", Events: []string{"session_start"}, Command: "sleep 0.3; echo ready", SendOutput: true}})
	defer a.hooks.Wait(0)
	a.Start("test")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := a.prepare(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
	if err := a.prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if out := a.hooks.DrainOutputs(a.sessionID()); len(out) != 1 || !strings.Contains(out[0].Output, "ready") {
		t.Fatalf("outputs = %+v", out)
	}
}
