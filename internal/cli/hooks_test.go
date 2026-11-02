package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// hooksSetup writes provider settings and a home dashy.toml recording events.
func hooksSetup(t *testing.T, status int) (record string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
	}))
	t.Cleanup(server.Close)
	config := fmt.Sprintf("provider='test'\nmodel='m'\n[providers.test]\nbase_url=%q\n", server.URL)
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(t.TempDir(), "events")
}

func hooksConfig(t *testing.T, record, serverURL string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "dashy.toml")
	text := fmt.Sprintf("provider='test'\nmodel='m'\n[providers.test]\nbase_url=%q\n[hooks.all]\nevents = [\"*\"]\ncommand = 'echo \"$DASHY_EVENT|$DASHY_MODE|$DASHY_EXIT_CODE\" >> %s'\n", serverURL, record)
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHeadlessInheritsGlobalHooksUnlessSupportDisabled(t *testing.T) {
	p := newProvider(t, nil)
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	record := filepath.Join(t.TempDir(), "events")
	tuiRecord := filepath.Join(t.TempDir(), "tui-events")

	// Implicit global hooks run in headless mode; the headless_support = false hook does not.
	homeCfg := fmt.Sprintf("provider='test'\nmodel='m'\n[providers.test]\nbase_url=%q\n"+
		"[hooks.implicit]\nevents = [\"*\"]\ncommand = 'echo \"$DASHY_EVENT|$DASHY_MODE|$DASHY_EXIT_CODE\" >> %s'\n"+
		"[hooks.tui_only]\nevents = [\"*\"]\ncommand = 'echo \"$DASHY_EVENT\" >> %s'\nheadless_support = false\n",
		p.server.URL, record, tuiRecord)
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(homeCfg), 0600); err != nil {
		t.Fatal(err)
	}
	runOnce := func(args ...string) {
		t.Helper()
		c := startJSON(t, args...)
		c.send(`{"id":"r1","type":"input","text":"hi"}`)
		c.await("done", doneFor(str(c.await("started", resultFor("r1")), "turn")))
		if err := c.finish(); err != nil {
			t.Fatal(err)
		}
	}
	want := "dashy_start|headless|\nsession_start|headless|\nturn_start|headless|\nsession_title_change|headless|\nmodel_request_start|headless|\nmodel_request_end|headless|\nturn_end|headless|\nsession_end|headless|\ndashy_exit|headless|0\n"

	runOnce()
	got, _ := os.ReadFile(record)
	if string(got) != want {
		t.Fatalf("implicit global hook:\ngot:\n%s\nwant:\n%s", got, want)
	}
	if tui, _ := os.ReadFile(tuiRecord); len(tui) != 0 {
		t.Fatalf("headless_support = false hook ran in headless mode: %q", tui)
	}

	// --no-extend with an explicit --config isolates the global hooks.
	os.Remove(record)
	explicitFile := hooksConfig(t, record, p.server.URL)
	runOnce("--config", explicitFile, "--no-extend")
	got, _ = os.ReadFile(record)
	if string(got) != want {
		t.Fatalf("--no-extend run:\ngot:\n%s\nwant:\n%s", got, want)
	}

	// Without --no-extend, explicit --config inherits global hooks too.
	os.Remove(record)
	runOnce("--config", explicitFile)
	got, _ = os.ReadFile(record)
	if strings.Count(string(got), "turn_start|headless|") != 2 {
		t.Fatalf("expected 2 turn_start events when global hooks inherited, got: %s", got)
	}
}

func TestInterfaceOnlyHooksNeverFireHeadless(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	record := filepath.Join(t.TempDir(), "events")
	file := filepath.Join(t.TempDir(), "dashy.toml")
	text := fmt.Sprintf("provider='test'\nmodel='m'\n[providers.test]\nbase_url=%q\n[hooks.all]\nevents = [\"idle\", \"steer_queued\", \"image_attached\"]\ncommand = 'echo \"$DASHY_EVENT|$DASHY_IMAGE_SOURCE\" >> %s'\n", p.server.URL, record)
	if err := os.WriteFile(file, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	c := startJSON(t, "--config", file, "--no-extend")
	c.send(`{"id":"r1","type":"input","text":"hi"}`)
	c.await("done", doneFor(str(c.await("started", resultFor("r1")), "turn")))
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(record); len(got) != 0 {
		t.Fatalf("interface-only hooks fired headless: %q", got)
	}
}

func TestDashyExitReportsTurnFailureInDone(t *testing.T) {
	p := newProvider(t, nil)
	p.server.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
	}))
	defer bad.Close()
	record := filepath.Join(t.TempDir(), "events")
	file := hooksConfig(t, record, bad.URL)
	c := startJSON(t, "--config", file, "--no-extend")
	c.send(`{"id":"r1","type":"input","text":"hi"}`)
	turn := str(c.await("started", resultFor("r1")), "turn")
	if d := c.await("error done", doneFor(turn)); d["status"] != "error" || d["error"] == "" {
		t.Fatalf("failed turn reported as %v", d)
	}
	if err := c.finish(); err != nil {
		t.Fatalf("a failed turn must not end the transport: %v", err)
	}
	got, _ := os.ReadFile(record)
	if !strings.HasSuffix(string(got), "session_end|headless|\ndashy_exit|headless|0\n") || !strings.Contains(string(got), "error|headless|\n") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestInvalidHooksFlagFailsStartup(t *testing.T) {
	hooksSetup(t, 200)
	var out, diagnostics bytes.Buffer
	for _, path := range []string{filepath.Join(t.TempDir(), "missing.toml"), func() string {
		p := filepath.Join(t.TempDir(), "bad.toml")
		_ = os.WriteFile(p, []byte("[hooks.bad]\nevents = [\"typo\"]\ncommand = \"true\"\n"), 0600)
		return p
	}()} {
		if err := Run(context.Background(), []string{"--mode", "jsonl", "--config", path}, strings.NewReader(""), &out, &diagnostics); err == nil {
			t.Fatalf("--config %s must fail", path)
		}
	}
}
