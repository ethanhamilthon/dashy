package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/agent"
	"dashy/internal/docs"
)

func TestHeadlessSessionsResumeAndFreshStart(t *testing.T) {
	p := newProvider(t, nil)
	home := p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"first"}`)
	first := str(c.await("first", resultFor("r1")), "turn")
	c.await("first done", doneFor(first))
	c.send(`{"id":"r2","type":"input","text":"second"}`)
	second := str(c.await("second", resultFor("r2")), "turn")
	c.await("second done", doneFor(second))
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	files, err := filepath.Glob(filepath.Join(home, "sessions", "*", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("sessions: %v %v", files, err)
	}
	id := strings.TrimSuffix(filepath.Base(files[0]), ".jsonl")

	resumed := startJSON(t, "--resume", id)
	state := resumed.await("ready", typeIs("ready"))
	if got := state["state"].(map[string]any)["session"].(map[string]any)["id"]; got != id {
		t.Fatalf("resume opened %v, want %s", got, id)
	}
	if err := resumed.finish(); err != nil {
		t.Fatal(err)
	}
	fresh := startJSON(t)
	fresh.await("ready", typeIs("ready"))
	if err := fresh.finish(); err != nil {
		t.Fatal(err)
	}
	if files, _ = filepath.Glob(filepath.Join(home, "sessions", "*", "*.jsonl")); len(files) != 1 {
		t.Fatalf("launch without input created a session: %v", files)
	}
}

func TestHeadlessRejectsRemovedPromptFlagAndModes(t *testing.T) {
	for _, args := range [][]string{{"--mode", "bad"}, {"unexpected"}, {"--prompt", "hi"}} {
		var out bytes.Buffer
		if err := Run(context.Background(), args, strings.NewReader(""), &out, &out); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	t.Setenv("DASHY_HOME", t.TempDir())
	var out, diagnostics bytes.Buffer
	if err := Run(context.Background(), []string{"--mode", "jsonl"}, strings.NewReader("bad-json\n"), &out, &diagnostics); err != nil {
		t.Fatalf("a bad record must not end the session: %v", err)
	}
	if !strings.Contains(out.String(), `"invalid_json"`) {
		t.Fatalf("protocol error missing: %s", out.String())
	}
}

func TestHeadlessCancellationWhileWaitingForInput(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	in, writer := io.Pipe()
	defer in.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []string{"--mode", "jsonl"}, in, io.Discard, io.Discard) }()
	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("input read blocked cancellation")
	}
}

func TestHeadlessInterruptStopsActiveWorkAndSavesSession(t *testing.T) {
	hold := make(chan struct{})
	p := newProvider(t, hold)
	home := p.configure(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		defer outWriter.Close()
		done <- Run(ctx, []string{"--mode", "jsonl"}, inReader, outWriter, io.Discard)
	}()
	c := &client{t: t, in: inWriter, done: make(chan error, 1), fresh: make(chan struct{}, 1)}
	go c.collect(outReader)
	c.send(`{"id":"r1","type":"input","text":"interrupt me"}`)
	turn := str(c.await("started", resultFor("r1")), "turn")
	<-p.started
	cancel()
	if err := <-done; err != context.Canceled {
		t.Fatalf("interrupted run returned %v", err)
	}
	if d := c.await("cancelled", doneFor(turn)); d["status"] != "cancelled" {
		t.Fatalf("done = %v", d)
	}
	if files, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*.jsonl")); len(files) != 1 {
		t.Fatalf("interrupted session not saved: %v", files)
	}
	close(hold)
	inWriter.Close()
}

func TestHeadlessRejectsOwnedSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	writer, err := agent.New(cwd, agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if err = writer.Save(); err != nil {
		t.Fatal(err)
	}
	id := writer.SessionInfo()["id"].(string)
	err = Run(context.Background(), []string{"--mode", "jsonl", "--resume", id}, strings.NewReader(""), io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "in use") {
		t.Fatalf("second client opened an owned session: %v", err)
	}
}

func TestShellCommandsUseEndpoint(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := agent.New(cwd, agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	var out bytes.Buffer
	if err = Run(context.Background(), []string{"docs", "runtime"}, strings.NewReader(""), &out, io.Discard); err != nil || !strings.Contains(out.String(), "dashy notify") {
		t.Fatalf("docs: %v %q", err, out.String())
	}
	t.Setenv("DASHY_ENDPOINT", "")
	if err = Run(context.Background(), []string{"notify", "hi"}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "-s") {
		t.Fatalf("notify outside a shell: %v", err)
	}
	result, err := a.Shell.Execute(context.Background(), `echo "$DASHY_ENDPOINT"`, time.Second)
	if err != nil || result.Error {
		t.Fatal(result, err)
	}
	t.Setenv("DASHY_ENDPOINT", strings.TrimSpace(result.Output))
	if err = Run(context.Background(), []string{"notify"}, strings.NewReader("from stdin"), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if messages := a.Notifications(); len(messages) != 1 || !strings.Contains(messages[0], "from stdin") {
		t.Fatalf("%v", messages)
	}
	path := filepath.Join(cwd, "image.png")
	data, _ := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYAAAAAYAAjCB0C8AAAAASUVORK5CYII=")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = Run(context.Background(), []string{"view", path, path, path, path}, strings.NewReader(""), io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if err = Run(context.Background(), []string{"view", path}, strings.NewReader(""), io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "four images") {
		t.Fatalf("view limit: %v", err)
	}
	if err = Run(context.Background(), []string{"view", filepath.Join(cwd, "missing.png")}, strings.NewReader(""), io.Discard, io.Discard); err == nil {
		t.Fatal("missing image accepted")
	}
}

func TestShortConfigFlag(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	config := filepath.Join(t.TempDir(), "tracker.toml")
	if err := os.WriteFile(config, []byte("system = 'You are Tracker'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, flag := range []string{"-c", "--config"} {
		t.Run(flag, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			if err := Run(context.Background(), []string{"--mode", "jsonl", flag, config}, strings.NewReader(""), &out, &diagnostics); err != nil {
				t.Fatalf("config flag failed: %v (%s)", err, diagnostics.String())
			}
			out.Reset()
			if err := Run(context.Background(), []string{"--mode", "jsonl", flag, config + ".missing"}, strings.NewReader(""), &out, &diagnostics); err == nil || !strings.Contains(err.Error(), ".missing") {
				t.Fatalf("missing explicit config must fail, got %v", err)
			}
		})
	}
}

// Every flag dashy defines must appear in the CLI reference.
func TestEveryFlagIsDocumented(t *testing.T) {
	var diagnostics bytes.Buffer
	if err := Run(context.Background(), []string{"-h"}, strings.NewReader(""), io.Discard, &diagnostics); err != nil {
		t.Fatal(err)
	}
	var doc bytes.Buffer
	if err := docs.Docs(&doc, []string{"cli"}); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, line := range strings.Split(diagnostics.String(), "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "-"); ok {
			found++
			name, _, _ = strings.Cut(name, " ")
			if !strings.Contains(doc.String(), "`-"+name) {
				t.Errorf("flag -%s is missing from dashy docs cli", name)
			}
		}
	}
	if found < 5 {
		t.Fatalf("parsed only %d flags from usage:\n%s", found, diagnostics.String())
	}
}
