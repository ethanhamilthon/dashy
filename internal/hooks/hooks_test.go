package hooks

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(data)
}

func TestFireRunsMatchingHooksInOrderWithVariables(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	r := &Runner{Mode: "tui"}
	r.Set([]Hook{
		{Events: []string{"turn_start", "turn_end"}, Command: `echo "$DASHY_EVENT $DASHY_MODE $X $(cat "$Y_FILE") $PWD" >> ` + out},
		{Events: []string{"idle"}, Command: "echo wrong >> " + out},
	})
	r.Fire("turn_start", map[string]string{"X": "1", "Y_FILE": "payload\nline2", "DASHY_CWD": dir})
	r.Fire("model_request_start", nil)
	r.Fire("turn_end", map[string]string{"X": "2", "Y_FILE": "p2", "DASHY_CWD": dir})
	r.Wait(5 * time.Second)
	got := strings.Split(strings.TrimSpace(read(t, out)), "\n")
	resolved, _ := filepath.EvalSymlinks(dir)
	if len(got) != 3 || !strings.HasPrefix(got[0], "turn_start tui 1 payload") || !strings.HasPrefix(got[1], "line2 ") || !strings.HasSuffix(got[1], resolved) ||
		!strings.HasPrefix(got[2], "turn_end tui 2 p2 ") || !strings.HasSuffix(got[2], resolved) {
		t.Fatalf("got %q", got)
	}
}

func TestWildcardAndSequenceVariables(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	r := &Runner{}
	r.Set([]Hook{{Events: []string{"*"}, Command: `echo "$DASHY_SEQ $DASHY_EVENT" >> ` + out + `; test -n "$DASHY_TS"`}})
	r.Fire("idle", nil)
	r.Fire("error", nil)
	r.Wait(5 * time.Second)
	lines := strings.Split(strings.TrimSpace(read(t, out)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " idle") || !strings.HasSuffix(lines[1], " error") || lines[0] >= lines[1] {
		t.Fatalf("lines = %q", lines)
	}
}

func TestPayloadFilesAreRemovedAfterHooksFinish(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{}
	r.Set([]Hook{{Events: []string{"idle"}, Command: `echo "$M_FILE" > ` + filepath.Join(dir, "path")}})
	r.Fire("idle", map[string]string{"M_FILE": "text"})
	r.Wait(5 * time.Second)
	path := strings.TrimSpace(read(t, filepath.Join(dir, "path")))
	if path == "" {
		t.Fatal("hook did not receive a path")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("payload file survived: %v", err)
	}
}

func TestFailuresAndTimeoutsAreLoggedAndDoNotBlock(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "hooks.log")
	r := &Runner{Log: log}
	r.Set([]Hook{
		{Events: []string{"idle"}, Command: "echo boom >&2; exit 3"},
		{Events: []string{"error"}, Command: "sleep 30", Timeout: 1},
	})
	started := time.Now()
	r.Fire("idle", nil)
	r.Fire("error", nil)
	if time.Since(started) > time.Second {
		t.Fatal("Fire blocked")
	}
	r.Wait(10 * time.Second)
	text := read(t, log)
	if !strings.Contains(text, "event=idle") || !strings.Contains(text, "exit status 3: boom") || !strings.Contains(text, "event=error") || !strings.Contains(text, "timed out after 1s") {
		t.Fatalf("log = %q", text)
	}
}

func TestSetKeepsQueuesOfIdenticalHooks(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	hook := Hook{Events: []string{"*"}, Command: `sleep 0.2; echo "$DASHY_EVENT" >> ` + out}
	r := &Runner{}
	r.Set([]Hook{hook})
	r.Fire("session_end", nil)
	r.Set([]Hook{hook}) // /new re-reads the same config
	r.Fire("session_start", nil)
	r.Wait(5 * time.Second)
	if got := read(t, out); got != "session_end\nsession_start\n" {
		t.Fatalf("order lost across Set: %q", got)
	}
}

func TestNilAndEmptyRunnersAreHarmless(t *testing.T) {
	var r *Runner
	r.Fire("idle", nil)
	r.Wait(time.Second)
	(&Runner{}).Fire("idle", nil)
}

func TestEveryEventIsDocumented(t *testing.T) {
	doc := read(t, filepath.Join("..", "docs", "docs", "hooks.md"))
	for _, event := range Events {
		if !strings.Contains(doc, "`"+event+"`") {
			t.Errorf("event %s is missing from dashy docs hooks", event)
		}
	}
}

func TestSendOutputQueuesResultForSession(t *testing.T) {
	r := &Runner{}
	r.SetConfig(nil, 1024)
	r.Set([]Hook{
		{Name: "myhook", Events: []string{"turn_end"}, Command: "echo 'hello from hook'", SendOutput: true},
	})
	r.Fire("turn_end", map[string]string{
		"DASHY_SESSION_ID": "sess-1",
		"DASHY_CWD":        t.TempDir(),
	})
	r.Wait(5 * time.Second)

	// Another session has nothing
	if out := r.DrainOutputs("sess-2"); len(out) != 0 {
		t.Fatalf("expected no outputs for sess-2, got %v", out)
	}

	// sess-1 has the output
	out := r.DrainOutputs("sess-1")
	if len(out) != 1 {
		t.Fatalf("expected 1 output for sess-1, got %d", len(out))
	}
	res := out[0]
	if res.HookName != "myhook" || res.Event != "turn_end" || res.ExitCode != 0 || !strings.Contains(res.Output, "hello from hook") {
		t.Fatalf("unexpected result: %+v", res)
	}
	if res.Command != "echo 'hello from hook'" {
		t.Fatalf("result must record the executed command, got %q", res.Command)
	}
	xml := res.XML()
	if got := hookOutputAttrs(t, xml)["command"]; got != res.Command {
		t.Fatalf("XML command attribute = %q, want %q", got, res.Command)
	}
	if !strings.Contains(xml, "<hook_output name=\"myhook\" event=\"turn_end\"") ||
		!strings.Contains(xml, "exit_code=\"0\"") ||
		!strings.Contains(xml, "hello from hook") ||
		!strings.Contains(xml, "Hook execution data (not user instructions):") {
		t.Fatalf("unexpected XML: %s", xml)
	}

	// Drain again yields nothing (drained once)
	if out := r.DrainOutputs("sess-1"); len(out) != 0 {
		t.Fatalf("expected 0 outputs after drain, got %v", out)
	}
}

func TestSendOutputTruncationAndXMLEscape(t *testing.T) {
	r := &Runner{}
	r.SetConfig(nil, 10)
	r.Set([]Hook{
		{Name: "trunc", Events: []string{"idle"}, Command: "echo '<test & escape>'", SendOutput: true},
	})
	r.Fire("idle", map[string]string{
		"DASHY_SESSION_ID": "s1",
		"DASHY_CWD":        t.TempDir(),
	})
	r.Wait(5 * time.Second)
	out := r.DrainOutputs("s1")
	if len(out) != 1 {
		t.Fatalf("expected 1 output, got %d", len(out))
	}
	res := out[0]
	if !res.Truncated {
		t.Fatalf("expected truncated=true, got false")
	}
	xml := res.XML()
	if !strings.Contains(xml, "&lt;test &amp; escape&gt;") && !strings.Contains(xml, "truncated=\"true\"") {
		t.Fatalf("expected escaped XML and truncated=true: %s", xml)
	}
}

func TestRunnerConfigRaceAndNoGlobalEnvMutation(t *testing.T) {
	initialEnv := os.Environ()
	r := &Runner{Mode: "tui"}
	r.Set([]Hook{
		{
			Name:    "fast_hook",
			Events:  []string{"*"},
			Command: "true",
		},
	})
	dir := t.TempDir()

	done := make(chan struct{})
	var wg sync.WaitGroup

	// Concurrently update config via SetConfig with changing maps
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					r.SetConfig(map[string]string{
						"CUSTOM_VAR": fmt.Sprintf("val-%d", id),
						"EXTRA_VAR":  "static",
					}, 1024+id)
				}
			}
		}(i)
	}

	// Concurrently fire events
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
					r.Fire("turn_start", map[string]string{
						"DASHY_SESSION_ID": fmt.Sprintf("session-%d", id),
						"DASHY_CWD":        dir,
					})
				}
			}
		}(i)
	}

	time.Sleep(100 * time.Millisecond)
	close(done)
	wg.Wait()
	r.Wait(2 * time.Second)

	currentEnv := os.Environ()
	if !slices.Equal(initialEnv, currentEnv) {
		t.Fatalf("global environment was mutated!\ninitial: %v\ncurrent: %v", initialEnv, currentEnv)
	}
}

func TestGlobalPendingOutputBound(t *testing.T) {
	logDir := t.TempDir()
	logFile := filepath.Join(logDir, "hooks.log")
	r := &Runner{
		Log:  logFile,
		Mode: "tui",
	}
	r.Set([]Hook{
		{
			Name:       "pnd",
			Events:     []string{"*"},
			Command:    "echo out",
			SendOutput: true,
		},
	})

	// Fill up to maxPendingGlobal (512 results across multiple sessions)
	for i := 0; i < maxPendingGlobal; i++ {
		sess := fmt.Sprintf("session-%d", i)
		r.recordOutput(Result{
			SessionID: sess,
			HookName:  "pnd",
			Event:     "test_event",
			Output:    "queued",
		})
	}

	// 513th output across any session must overflow and be logged
	r.recordOutput(Result{
		SessionID: "new-session",
		HookName:  "pnd",
		Event:     "test_event",
		Output:    "should-drop",
	})

	logData, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected log file to exist: %v", err)
	}
	if !strings.Contains(string(logData), "global pending output queue full") {
		t.Fatalf("expected global pending output queue full message, got: %s", string(logData))
	}

	// Draining a session frees global capacity
	drained := r.DrainOutputs("session-0")
	if len(drained) != 1 {
		t.Fatalf("expected 1 drained output, got %d", len(drained))
	}

	// Now another output can be recorded without overflowing
	r.recordOutput(Result{
		SessionID: "new-session",
		HookName:  "pnd",
		Event:     "test_event",
		Output:    "now-accepted",
	})
	drainedNew := r.DrainOutputs("new-session")
	if len(drainedNew) != 1 || drainedNew[0].Output != "now-accepted" {
		t.Fatalf("expected output to be accepted after drain, got %+v", drainedNew)
	}
}

// hookOutputAttrs decodes the single hook_output start tag in text, failing on malformed XML.
func hookOutputAttrs(t *testing.T, text string) map[string]string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(text))
	var found map[string]string
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("invalid XML: %v\n%s", err, text)
		}
		if start, ok := tok.(xml.StartElement); ok && start.Name.Local == "hook_output" {
			if found != nil {
				t.Fatalf("expected one hook_output element: %s", text)
			}
			found = map[string]string{}
			for _, a := range start.Attr {
				found[a.Name.Local] = a.Value
			}
		}
	}
	if found == nil {
		t.Fatalf("no hook_output element: %s", text)
	}
	return found
}

func TestHookOutputCommandXML(t *testing.T) {
	start := time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct{ name, command, want string }{
		{"quotes", `echo "double" 'single'`, `echo "double" 'single'`},
		{"ampersand", `make a && make b || true`, `make a && make b || true`},
		{"angle brackets", `cat <input >output 2>&1 </dev/null`, `cat <input >output 2>&1 </dev/null`},
		{"token literal", `curl -H "Authorization: $TOKEN" "$URL"`, `curl -H "Authorization: $TOKEN" "$URL"`},
		{"tabs and newlines", "line one\n\tline two\r\nline three", "line one\n\tline two\r\nline three"},
		{"unicode", "echo '日本語 🚀 café'", "echo '日本語 🚀 café'"},
		{"empty", "", ""},
		{"control characters removed", "echo\x01 hi\x1b[0m", "echo hi[0m"},
		{"invalid utf8 removed", "bad\xffbyte", "badbyte"},
		{"noncharacter removed", "x\uFFFEy", "xy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Result{HookName: "cmd", Event: "idle", StartTime: start, Output: "ok", Command: tc.command}
			attrs := hookOutputAttrs(t, res.XML())
			got, ok := attrs["command"]
			if !ok {
				t.Fatalf("command attribute missing: %s", res.XML())
			}
			if got != tc.want {
				t.Fatalf("command = %q, want %q", got, tc.want)
			}
		})
	}
}

// A result keeps the command that actually ran, even if reload replaces the hook under the same name.
func TestHookOutputKeepsExecutedCommandAfterReload(t *testing.T) {
	r := &Runner{}
	r.SetConfig(nil, 1024)
	r.Set([]Hook{{Name: "context", Events: []string{"idle"}, Command: "echo old-version", SendOutput: true}})
	r.Fire("idle", map[string]string{"DASHY_SESSION_ID": "sess-reload", "DASHY_CWD": t.TempDir()})
	r.Set([]Hook{{Name: "context", Events: []string{"idle"}, Command: "echo new-version", SendOutput: true}})
	r.Wait(5 * time.Second)

	out := r.DrainOutputs("sess-reload")
	if len(out) != 1 {
		t.Fatalf("expected 1 output, got %d", len(out))
	}
	if out[0].Command != "echo old-version" || !strings.Contains(out[0].Output, "old-version") {
		t.Fatalf("result must keep the executed command: %+v", out[0])
	}
	if got := hookOutputAttrs(t, out[0].XML())["command"]; got != "echo old-version" {
		t.Fatalf("XML command = %q, want the executed command", got)
	}
}

// The mandatory session_start job gates WaitStartup; ordinary session_start hooks do not, and nothing runs twice.
func TestWaitStartupGatesOnlyMandatorySessionStart(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	r := &Runner{}
	r.Set([]Hook{
		{Name: "boot", Events: []string{"session_start"}, Command: "sleep 0.3; echo ready; echo x >> " + count, SendOutput: true},
		{Name: "lazy", Events: []string{"session_start"}, Command: "sleep 30"},
	})
	defer r.Wait(0)
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": dir})
	started := time.Now()
	if err := r.WaitStartup(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("startup gate waited %v", elapsed)
	}
	if out := r.DrainOutputs("s1"); len(out) != 1 || !strings.Contains(out[0].Output, "ready") {
		t.Fatalf("outputs = %+v", out)
	}
	if got := read(t, count); got != "x\n" {
		t.Fatalf("bootstrap ran more than once: %q", got)
	}
}

// A failed startup stays sticky for the session until a reload retries it with a fixed hook.
func TestStartupFailurePersistsUntilReloadRetries(t *testing.T) {
	dir := t.TempDir()
	boot := func(cmd string) Hook {
		return Hook{Name: "boot", Events: []string{"session_start"}, Command: cmd, SendOutput: true}
	}
	r := &Runner{}
	r.Set([]Hook{boot("echo broken; exit 3")})
	defer r.Wait(0)
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": dir})
	ctx := context.Background()
	if err := r.WaitStartup(ctx, "s1"); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("err = %v", err)
	}
	if err := r.WaitStartup(ctx, "s1"); err == nil {
		t.Fatal("failure did not persist for the session")
	}
	r.Set([]Hook{boot("echo fixed")})
	if err := r.WaitStartup(ctx, "s1"); err != nil {
		t.Fatalf("reload did not retry the startup: %v", err)
	}
	fixed := false
	for _, res := range r.DrainOutputs("s1") {
		fixed = fixed || strings.Contains(res.Output, "fixed")
	}
	if !fixed {
		t.Fatal("retried startup output was not queued")
	}
}

// A startup job that cannot be queued fails its session immediately instead of hanging.
func TestStartupQueueRejectionBlocks(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{}
	r.Set([]Hook{{Name: "boot", Events: []string{"*"}, Command: "sleep 2", SendOutput: true}})
	defer r.Wait(0)
	r.Fire("idle", map[string]string{"DASHY_CWD": dir}) // occupies the worker
	for deadline := time.Now().Add(5 * time.Second); len(r.workers[0].jobs) > 0 && time.Now().Before(deadline); {
		time.Sleep(time.Millisecond)
	}
	for i := 0; i < queueSize; i++ {
		r.Fire("idle", map[string]string{"DASHY_CWD": dir})
	}
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "boot", "DASHY_CWD": dir})
	if err := r.WaitStartup(context.Background(), "boot"); err == nil || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("err = %v", err)
	}
}

// A startup hook that exceeds its timeout blocks the session with that failure.
func TestStartupTimeoutBlocks(t *testing.T) {
	r := &Runner{}
	r.Set([]Hook{{Name: "boot", Events: []string{"session_start"}, Command: "sleep 30", SendOutput: true, Timeout: 1}})
	defer r.Wait(0)
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": t.TempDir()})
	started := time.Now()
	if err := r.WaitStartup(context.Background(), "s1"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("timeout did not bound the wait")
	}
}

// Canceling the wait returns the context error, and the result is still available afterwards.
func TestWaitStartupCancellationKeepsResult(t *testing.T) {
	r := &Runner{}
	r.SetConfig(nil, 1024)
	r.Set([]Hook{{Name: "boot", Events: []string{"session_start"}, Command: "sleep 0.3; echo ready", SendOutput: true}})
	defer r.Wait(0)
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": t.TempDir()})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := r.WaitStartup(ctx, "s1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline", err)
	}
	if err := r.WaitStartup(context.Background(), "s1"); err != nil {
		t.Fatal(err)
	}
	if out := r.DrainOutputs("s1"); len(out) != 1 || !strings.Contains(out[0].Output, "ready") {
		t.Fatalf("outputs = %+v", out)
	}
}

func TestSessionEndClearsStartupState(t *testing.T) {
	r := &Runner{}
	r.Set([]Hook{{Events: []string{"session_start"}, Command: "true", SendOutput: true}})
	vars := map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": t.TempDir()}
	r.Fire("session_start", vars)
	r.Fire("session_end", vars)
	r.Wait(5 * time.Second)
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.startups) != 0 {
		t.Fatalf("startup state of an ended session survived: %d", len(r.startups))
	}
}

// Wait kills the running hook's process group after the grace period and skips the queued job.
func TestWaitKillsActiveHookAndSkipsQueued(t *testing.T) {
	dir := t.TempDir()
	logPath, ran, pidPath := filepath.Join(dir, "hooks.log"), filepath.Join(dir, "ran"), filepath.Join(dir, "pid")
	r := &Runner{Log: logPath}
	r.Set([]Hook{{Name: "slow", Events: []string{"*"}, Command: `echo "$DASHY_EVENT" >> ` + ran +
		`; if [ "$DASHY_EVENT" = first ]; then echo $$ > ` + pidPath + `; exec sleep 30; fi`}})
	r.Fire("first", map[string]string{"DASHY_CWD": dir})
	for deadline := time.Now().Add(5 * time.Second); read(t, pidPath) == "" && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	r.Fire("second", map[string]string{"DASHY_CWD": dir})
	started := time.Now()
	r.Wait(200 * time.Millisecond)
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Wait took %v", elapsed)
	}
	if got := read(t, ran); got != "first\n" {
		t.Fatalf("ran = %q; the queued job must be skipped", got)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(read(t, pidPath)))
	if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("hook process %d survived shutdown: %v", pid, err)
	}
	if text := read(t, logPath); !strings.Contains(text, "terminated at shutdown") || !strings.Contains(text, "skipped, hooks shut down") {
		t.Fatalf("log = %q", text)
	}
	// The runner is reusable after a forced Wait.
	r.Set([]Hook{{Events: []string{"idle"}, Command: "echo again > " + ran}})
	r.Fire("idle", map[string]string{"DASHY_CWD": dir})
	r.Wait(5 * time.Second)
	if got := read(t, ran); got != "again\n" {
		t.Fatalf("runner not reusable after Wait: %q", got)
	}
}

func TestHooksGetConfiguredEnvAndDashyOnPATH(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	r := &Runner{}
	r.SetConfig(map[string]string{"DASHY_TEST_CONFIGURED": "yes", "PATH": "/usr/bin:/bin"}, 0)
	r.Set([]Hook{{Events: []string{"idle"}, Command: `printf '%s|%s' "$DASHY_TEST_CONFIGURED" "$PATH" > ` + out}})
	r.Fire("idle", map[string]string{"DASHY_CWD": dir})
	r.Wait(5 * time.Second)
	if want := "yes|" + filepath.Dir(exe) + ":/usr/bin:/bin"; read(t, out) != want {
		t.Fatalf("got %q, want %q", read(t, out), want)
	}
}

// The startup budget covers the jobs queued ahead on the same hook. A slow dashy_start on a
// wildcard send_output hook must not make a legitimate session_start fail.
func TestStartupBudgetCoversQueuedJobsAhead(t *testing.T) {
	r := &Runner{}
	r.Set([]Hook{{Name: "all", Events: []string{"*"}, SendOutput: true, Timeout: 3,
		Command: `if [ "$DASHY_EVENT" = dashy_start ]; then sleep 2.8; else sleep 1.5; fi; echo "$DASHY_EVENT"`}})
	defer r.Wait(0)
	vars := map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": t.TempDir()}
	r.Fire("dashy_start", vars)
	r.Fire("session_start", vars)
	if err := r.WaitStartup(context.Background(), "s1"); err != nil {
		t.Fatalf("legitimate session_start failed behind a slow dashy_start: %v", err)
	}
}

// The retry uses a snapshot of the session_start variables, so later caller edits do not leak in.
func TestStartupRetryUsesVariableSnapshot(t *testing.T) {
	dir := t.TempDir()
	boot := func(cmd string) Hook {
		return Hook{Name: "boot", Events: []string{"session_start"}, Command: cmd, SendOutput: true}
	}
	r := &Runner{}
	r.Set([]Hook{boot("exit 3")})
	defer r.Wait(0)
	vars := map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": dir}
	r.Fire("session_start", vars)
	if err := r.WaitStartup(context.Background(), "s1"); err == nil {
		t.Fatal("expected failure")
	}
	vars["DASHY_SESSION_ID"] = "other"
	r.Set([]Hook{boot("true")})
	if err := r.WaitStartup(context.Background(), "s1"); err != nil {
		t.Fatalf("retry did not run for the snapshot session: %v", err)
	}
}

// After a failed bootstrap, fixing the hook and reloading delivers only the successful startup
// output. Other events queued for the session are kept.
func TestRetryDeliversOnlySuccessfulStartupOutput(t *testing.T) {
	boot := func(cmd string) Hook {
		return Hook{Name: "boot", Events: []string{"session_start"}, Command: cmd, SendOutput: true}
	}
	r := &Runner{}
	r.Set([]Hook{boot("echo broken; exit 3")})
	defer r.Wait(0)
	ctx := context.Background()
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "s1", "DASHY_CWD": t.TempDir()})
	if err := r.WaitStartup(ctx, "s1"); err == nil || !strings.Contains(err.Error(), "/reload") {
		t.Fatalf("err = %v, want a reload hint", err)
	}
	r.recordOutput(Result{SessionID: "s1", Event: "turn_end", Output: "kept"})
	r.Set([]Hook{boot("echo fixed")})
	if err := r.WaitStartup(ctx, "s1"); err != nil {
		t.Fatalf("reload did not retry: %v", err)
	}
	var got []string
	for _, res := range r.DrainOutputs("s1") {
		got = append(got, res.Event+":"+strings.TrimSpace(res.Output))
	}
	if want := "turn_end:kept|session_start:fixed"; strings.Join(got, "|") != want {
		t.Fatalf("delivered %q, want %q", got, want)
	}
}
