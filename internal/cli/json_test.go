package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"dashy/internal/agent"
)

// provider is a Responses stub. With hold set, streamed answers wait for the
// test; every request body is recorded for payload checks.
type provider struct {
	server  *httptest.Server
	hold    chan struct{}
	started chan struct{}
	listed  chan struct{} // one value per model listing request that reaches the provider
	mu      sync.Mutex
	bodies  []string
}

func newProvider(t *testing.T, hold chan struct{}) *provider {
	t.Helper()
	p := &provider{hold: hold, started: make(chan struct{}, 64), listed: make(chan struct{}, 64)}
	p.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method == http.MethodGet {
			select {
			case p.listed <- struct{}{}:
			default:
			}
			if p.hold != nil {
				select {
				case <-p.hold:
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"data":[{"id":"m"},{"id":"other"}]}`)
			return
		}
		if !bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"id":"s","object":"response","output":[{"type":"message","id":"m","role":"assistant","status":"completed","content":[{"type":"output_text","text":"summary text","annotations":[]}]}]}`)
			return
		}
		p.mu.Lock()
		p.bodies = append(p.bodies, string(body))
		p.mu.Unlock()
		select {
		case p.started <- struct{}{}:
		default:
		}
		if p.hold != nil {
			select {
			case <-p.hold:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"ans\"}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\"}]}]}}\n\n")
	}))
	t.Cleanup(p.server.Close)
	return p
}

// configure points DASHY_HOME at a config that uses the stub.
func (p *provider) configure(t *testing.T, extra string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	config := fmt.Sprintf("provider='test'\nmodel='m'\n%s[providers.test]\nbase_url=%q\n", extra, p.server.URL)
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

func (p *provider) requestBodies() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.bodies...)
}

// client drives Run over pipes: it writes records to stdin and collects every
// stdout line in order.
type client struct {
	t      *testing.T
	in     *io.PipeWriter
	cancel context.CancelFunc
	done   chan error
	mu     sync.Mutex
	lines  []map[string]any
	fresh  chan struct{}
	exited bool
	err    error
}

func startJSON(t *testing.T, args ...string) *client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inReader, inWriter := io.Pipe()
	outReader, outWriter := io.Pipe()
	c := &client{t: t, in: inWriter, cancel: cancel, done: make(chan error, 1), fresh: make(chan struct{}, 1)}
	go func() {
		defer outWriter.Close()
		c.done <- Run(ctx, append([]string{"--mode", "jsonl"}, args...), inReader, outWriter, io.Discard)
	}()
	go func() {
		scanner := bufio.NewScanner(outReader)
		scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
		for scanner.Scan() {
			var record map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
				record = map[string]any{"type": "invalid-output", "raw": scanner.Text()}
			}
			c.mu.Lock()
			c.lines = append(c.lines, record)
			c.mu.Unlock()
			select {
			case c.fresh <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		inWriter.Close()
		cancel()
		c.waitExit(time.Second)
	})
	return c
}

// waitExit waits for Run to return without closing stdin. It reports whether it did.
func (c *client) waitExit(limit time.Duration) bool {
	c.mu.Lock()
	if c.exited {
		c.mu.Unlock()
		return true
	}
	c.mu.Unlock()
	select {
	case err := <-c.done:
		c.mu.Lock()
		c.exited, c.err = true, err
		c.mu.Unlock()
		return true
	case <-time.After(limit):
		return false
	}
}

// send writes one record followed by a newline.
func (c *client) send(record string) {
	c.t.Helper()
	if _, err := fmt.Fprintln(c.in, record); err != nil {
		c.t.Fatal(err)
	}
}

// await returns the first output record, from the start of the stream, matching pred.
func (c *client) await(what string, pred func(map[string]any) bool) map[string]any {
	c.t.Helper()
	deadline := time.After(10 * time.Second)
	next := 0
	for {
		c.mu.Lock()
		for ; next < len(c.lines); next++ {
			if pred(c.lines[next]) {
				found := c.lines[next]
				c.mu.Unlock()
				return found
			}
		}
		c.mu.Unlock()
		select {
		case <-c.fresh:
		case <-deadline:
			c.t.Fatalf("timed out waiting for %s; output: %v", what, c.snapshot())
		}
	}
}

func (c *client) snapshot() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.lines...)
}

// count returns how many output records match pred so far.
func (c *client) count(pred func(map[string]any) bool) int {
	n := 0
	for _, line := range c.snapshot() {
		if pred(line) {
			n++
		}
	}
	return n
}

// collect reads transport lines from r into the client, as startJSON does.
func (c *client) collect(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			record = map[string]any{"type": "invalid-output", "raw": scanner.Text()}
		}
		c.mu.Lock()
		c.lines = append(c.lines, record)
		c.mu.Unlock()
		select {
		case c.fresh <- struct{}{}:
		default:
		}
	}
}

// finish closes stdin and returns Run's result.
func (c *client) finish() error {
	c.t.Helper()
	c.in.Close()
	if !c.waitExit(10 * time.Second) {
		c.t.Fatal("dashy did not exit after stdin closed")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func resultFor(id string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == "result" && m["id"] == id }
}

func doneFor(turn string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == "done" && m["turn"] == turn }
}

func typeIs(kind string) func(map[string]any) bool {
	return func(m map[string]any) bool { return m["type"] == kind }
}

func str(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func TestJSONProtocolLaunchWaitsForInputWithoutWork(t *testing.T) {
	p := newProvider(t, nil)
	home := p.configure(t, "")
	c := startJSON(t)
	ready := c.await("ready", typeIs("ready"))
	if ready["protocol"] != float64(protocolVersion) || ready["seq"] != float64(1) {
		t.Fatalf("ready = %v", ready)
	}
	time.Sleep(50 * time.Millisecond)
	if len(p.requestBodies()) != 0 || c.count(typeIs("started")) != 0 {
		t.Fatal("launch started work")
	}
	if files, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*.jsonl")); len(files) != 0 {
		t.Fatalf("waiting created a session file: %v", files)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	if got := c.snapshot(); str(got[len(got)-1], "reason") != "eof" {
		t.Fatalf("last record = %v", got[len(got)-1])
	}
}

func TestJSONProtocolInputStreamsOrderedEventsAndOneDone(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"hello"}`)
	res := c.await("result", resultFor("r1"))
	if res["ok"] != true || res["status"] != "started" || res["turn"] == nil {
		t.Fatalf("input result = %v", res)
	}
	turn := str(res, "turn")
	done := c.await("done", doneFor(turn))
	if done["status"] != "ok" || done["error"] != "" {
		t.Fatalf("done = %v", done)
	}
	if n := c.count(doneFor(turn)); n != 1 {
		t.Fatalf("done records = %d", n)
	}
	last := float64(0)
	sawDelta, sawAnswer := false, false
	for _, line := range c.snapshot() {
		seq, ok := line["seq"].(float64)
		if !ok || seq <= last {
			t.Fatalf("sequence not monotonic at %v", line)
		}
		last = seq
		if event, ok := line["event"].(map[string]any); ok {
			sawDelta = sawDelta || event["kind"] == "delta" && event["provisional"] == true
			sawAnswer = sawAnswer || event["kind"] == "assistant" && event["final"] == true
		}
	}
	if !sawDelta || !sawAnswer {
		t.Fatalf("agent event fields missing: %v", c.snapshot())
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolSteerStopAndKeepRunningWithoutRestart(t *testing.T) {
	hold := make(chan struct{})
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"long task"}`)
	turn := str(c.await("started", resultFor("r1")), "turn")
	<-p.started
	c.send(`{"id":"r2","type":"steer","text":"focus on errors"}`)
	if res := c.await("steer result", resultFor("r2")); res["status"] != "queued" {
		t.Fatalf("steer = %v", res)
	}
	c.send(`{"id":"r3","type":"recall"}`)
	c.await("recall result", resultFor("r3"))
	c.send(`{"id":"r4","type":"steer","text":"second thought"}`)
	c.await("second steer", resultFor("r4"))
	c.send(`{"id":"r5","type":"stop"}`)
	if res := c.await("stop ack", resultFor("r5")); res["status"] != "cancelling" {
		t.Fatalf("stop = %v", res)
	}
	done := c.await("cancelled done", doneFor(turn))
	if done["status"] != "cancelled" {
		t.Fatalf("done = %v", done)
	}
	unsent, _ := done["unsent"].([]any)
	if len(unsent) != 1 || unsent[0].(map[string]any)["text"] != "second thought" {
		t.Fatalf("unsent = %v", done["unsent"])
	}
	c.send(`{"id":"r6","type":"rename","args":{"title":"Review"}}`)
	c.await("rename", resultFor("r6"))
	c.send(`{"id":"r7","type":"new"}`)
	c.await("new", resultFor("r7"))
	close(hold)
	c.send(`{"id":"r8","type":"input","text":"again"}`)
	next := str(c.await("second turn", resultFor("r8")), "turn")
	if d := c.await("second done", doneFor(next)); d["status"] != "ok" {
		t.Fatalf("second turn = %v", d)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolRejectsMalformedRecordsAndKeepsSessionUsable(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{not json`)
	if e := c.await("invalid_json", typeIs("error")); e["error"].(map[string]any)["code"] != "invalid_json" {
		t.Fatalf("error = %v", e)
	}
	c.send(`{"type":"input","text":"no id"}`)
	c.await("missing_id", typeIs("error"))
	c.send(`{"id":"x1","type":"launch-rockets"}`)
	if res := c.await("unknown type", resultFor("x1")); res["ok"] != false {
		t.Fatalf("unknown type = %v", res)
	}
	c.send(`{"id":"x2","type":"steer","text":"no turn"}`)
	if res := c.await("steer idle", resultFor("x2")); res["error"].(map[string]any)["code"] != "not_busy" {
		t.Fatalf("steer while idle = %v", res)
	}
	c.send(`{"id":"x3","type":"input"}`)
	if res := c.await("empty input", resultFor("x3")); res["error"].(map[string]any)["code"] != "invalid" {
		t.Fatalf("empty input = %v", res)
	}
	c.send(`{"id":"x4","type":"command","name":"/bogus"}`)
	if res := c.await("unknown command", resultFor("x4")); res["error"].(map[string]any)["code"] != "unknown_command" {
		t.Fatalf("unknown command = %v", res)
	}
	c.send(`{"id":"ok","type":"state"}`)
	if res := c.await("state after errors", resultFor("ok")); res["ok"] != true {
		t.Fatalf("session unusable after errors: %v", res)
	}
	if len(p.requestBodies()) != 0 {
		t.Fatal("invalid records reached the model")
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolOversizedRecordIsSkippedAndReported(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	c := startJSON(t)
	big := `{"id":"huge","type":"input","text":"` + strings.Repeat("a", maxRecordBytes+10) + `"}`
	c.send(big)
	c.await("record_too_large", func(m map[string]any) bool {
		e, _ := m["error"].(map[string]any)
		return m["type"] == "error" && e["code"] == "record_too_large"
	})
	c.send(`{"id":"after","type":"state"}`)
	if res := c.await("state", resultFor("after")); res["ok"] != true {
		t.Fatalf("stream out of sync after oversized record: %v", res)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolBusyGuardsAndDuplicateIDs(t *testing.T) {
	hold := make(chan struct{})
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"busy"}`)
	turn := str(c.await("started", resultFor("r1")), "turn")
	<-p.started
	for i, record := range []string{`{"id":"b1","type":"reload"}`, `{"id":"b2","type":"select_model","args":{"provider":"test","model":"other"}}`, `{"id":"b3","type":"compact"}`, `{"id":"b4","type":"add_provider","args":{"name":"x","base_url":"http://x"}}`} {
		c.send(record)
		res := c.await(fmt.Sprintf("guard %d", i), resultFor(fmt.Sprintf("b%d", i+1)))
		if res["error"].(map[string]any)["code"] != "busy" {
			t.Fatalf("guard %d = %v", i, res)
		}
	}
	close(hold)
	c.await("done", doneFor(turn))
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolRejectsDuplicateInFlightID(t *testing.T) {
	hold := make(chan struct{})
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"m1","type":"models"}`)
	c.send(`{"id":"m1","type":"models"}`)
	dup := c.await("duplicate", resultFor("m1"))
	if dup["ok"] != false || dup["error"].(map[string]any)["code"] != "duplicate_id" {
		t.Fatalf("duplicate in-flight id = %v", dup)
	}
	close(hold)
	first := c.await("first listing", func(m map[string]any) bool {
		return m["type"] == "result" && m["id"] == "m1" && m["ok"] == true
	})
	if first["status"] != "ok" {
		t.Fatalf("first listing = %v", first)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolEOFDrainsAcceptedWork(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"last words"}`)
	turn := str(c.await("started", resultFor("r1")), "turn")
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	if d := c.await("done after eof", doneFor(turn)); d["status"] != "ok" {
		t.Fatalf("accepted work was not drained: %v", d)
	}
	if got := c.snapshot(); str(got[len(got)-1], "reason") != "eof" {
		t.Fatalf("shutdown reason: %v", got[len(got)-1])
	}
}

func TestJSONProtocolQuitCancelsActiveWorkAndExits(t *testing.T) {
	hold := make(chan struct{})
	p := newProvider(t, hold)
	home := p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"r1","type":"input","text":"work"}`)
	turn := str(c.await("started", resultFor("r1")), "turn")
	<-p.started
	c.send(`{"id":"r2","type":"quit"}`)
	if res := c.await("quit", resultFor("r2")); res["status"] != "deferred" {
		t.Fatalf("quit = %v", res)
	}
	if d := c.await("cancelled", doneFor(turn)); d["status"] != "cancelled" {
		t.Fatalf("done = %v", d)
	}
	if !c.waitExit(5 * time.Second) {
		t.Fatal("quit did not end the process")
	}
	if c.err != nil {
		t.Fatal(c.err)
	}
	if got := c.snapshot(); str(got[len(got)-1], "reason") != "quit" {
		t.Fatalf("shutdown = %v", got[len(got)-1])
	}
	if files, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*.jsonl")); len(files) != 1 {
		t.Fatalf("quit did not save the interrupted session: %v", files)
	}
	close(hold)
}

func TestJSONProtocolCredentialsAreNeverEchoed(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"p1","type":"add_provider","args":{"name":"vault","base_url":"https://user:pw@example.test/v1?token=abc","api_key":"sk-live-secret"}}`)
	res := c.await("add", resultFor("p1"))
	if res["ok"] != true {
		t.Fatalf("add_provider = %v", res)
	}
	c.send(`{"id":"p2","type":"add_provider","args":{"name":"vault","base_url":"https://example.test","api_key":"sk-live-secret"}}`)
	dup := c.await("duplicate", resultFor("p2"))
	c.send(`{"id":"p3","type":"providers"}`)
	c.await("providers", resultFor("p3"))
	c.send(`{"id":"p4","type":"models"}`)
	c.await("models", resultFor("p4"))
	raw := fmt.Sprint(c.snapshot())
	if strings.Contains(raw, "sk-live-secret") || strings.Contains(raw, "user:pw") || strings.Contains(raw, "token=abc") {
		t.Fatalf("credential leaked: %s", raw)
	}
	if dup["ok"] != false {
		t.Fatalf("duplicate provider accepted: %v", dup)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolFilesAreSnapshottedAndSentWithPrompt(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	project := t.TempDir()
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("alpha <tag>"), 0600); err != nil {
		t.Fatal(err)
	}
	c := startJSON(t)
	c.send(`{"id":"f1","type":"input","text":"review","files":["notes.txt"]}`)
	turn := str(c.await("started", resultFor("f1")), "turn")
	c.await("done", doneFor(turn))
	bodies := p.requestBodies()
	if len(bodies) == 0 {
		t.Fatal("no model request")
	}
	var payload struct {
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil || len(payload.Input) == 0 {
		t.Fatalf("payload: %v", err)
	}
	if text := payload.Input[0].Content; !strings.Contains(text, `<file path="notes.txt"`) || !strings.Contains(text, "alpha &lt;tag&gt;") {
		t.Fatalf("file content missing from payload: %q", text)
	}
	c.send(`{"id":"f2","type":"input","text":"outside","files":["../../etc/passwd"]}`)
	if res := c.await("outside", resultFor("f2")); res["error"].(map[string]any)["code"] != "invalid_file" {
		t.Fatalf("outside file accepted: %v", res)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolFileLimitComesFromConfig(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "max_file_bytes = 5\n")
	project := t.TempDir()
	t.Chdir(project)
	if err := os.WriteFile(filepath.Join(project, "notes.txt"), []byte("abcdefghij"), 0600); err != nil {
		t.Fatal(err)
	}
	c := startJSON(t)
	ready := c.await("ready", typeIs("ready"))
	if caps, _ := ready["capabilities"].(map[string]any); caps["max_file_bytes"] != float64(5) {
		t.Fatalf("ready capabilities ignore max_file_bytes: %v", ready["capabilities"])
	}
	c.send(`{"id":"f1","type":"input","text":"review","files":["notes.txt"]}`)
	turn := str(c.await("started", resultFor("f1")), "turn")
	c.await("done", doneFor(turn))
	bodies := p.requestBodies()
	if len(bodies) == 0 {
		t.Fatal("no model request")
	}
	var payload struct {
		Input []struct {
			Content string `json:"content"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(bodies[0]), &payload); err != nil || len(payload.Input) == 0 {
		t.Fatalf("payload: %v", err)
	}
	text := payload.Input[0].Content
	if !strings.Contains(text, `included="5" truncated="true"`) || !strings.Contains(text, "abcde") || strings.Contains(text, "fghij") {
		t.Fatalf("file limit not applied from config: %q", text)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONProtocolCommandsAndReadOnlyRequests(t *testing.T) {
	p := newProvider(t, nil)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"c1","type":"command","name":"/rename","args":{"title":"Plan"}}`)
	c.await("rename", resultFor("c1"))
	c.send(`{"id":"c2","type":"sessions"}`)
	if res := c.await("sessions", resultFor("c2")); res["ok"] != true {
		t.Fatalf("sessions = %v", res)
	}
	c.send(`{"id":"c3","type":"history"}`)
	c.await("history", resultFor("c3"))
	c.send(`{"id":"c4","type":"command","name":"/help"}`)
	help := c.await("help", resultFor("c4"))
	if commands := help["data"].(map[string]any)["commands"].([]any); len(commands) != 9 {
		t.Fatalf("commands = %v", commands)
	}
	c.send(`{"id":"c5","type":"command","name":"/model"}`)
	models := c.await("models", resultFor("c5"))
	if list := models["data"].(map[string]any)["models"].([]any); len(list) != 2 {
		t.Fatalf("models = %v", models)
	}
	c.send(`{"id":"c6","type":"select_model","args":{"provider":"test","model":"other","effort":"high"}}`)
	c.await("select", resultFor("c6"))
	c.send(`{"id":"c7","type":"command","name":"/reload"}`)
	c.await("reload", resultFor("c7"))
	c.send(`{"id":"c8","type":"command","name":"/resume"}`)
	if res := c.await("resume without id", resultFor("c8")); res["error"].(map[string]any)["code"] != "invalid" {
		t.Fatalf("resume without id = %v", res)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

// blockingWriter accepts nothing until release is closed, like a stdout pipe nobody reads.
type blockingWriter struct{ release chan struct{} }

func (w blockingWriter) Write(p []byte) (int, error) {
	<-w.release
	return len(p), nil
}

// failingWriter reports a write error on every call, like a closed stdout.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestJSONProtocolOutputBacklogOverflowStopsTheSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	release := make(chan struct{})
	defer close(release)
	var input bytes.Buffer
	for i := 0; i < outboxLimit+200; i++ {
		fmt.Fprintf(&input, "{\"id\":\"s%d\",\"type\":\"state\"}\n", i)
	}
	done := make(chan error, 1)
	go func() {
		done <- serveJSON(context.Background(), openTestAgent(t), t.TempDir(), &input, blockingWriter{release}, io.Discard)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "backlog") {
			t.Fatalf("overflow ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a full output backlog blocked the stdin reader")
	}
}

func TestJSONProtocolBrokenStdoutEndsTheSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	done := make(chan error, 1)
	go func() {
		done <- serveJSON(context.Background(), openTestAgent(t), t.TempDir(), strings.NewReader(""), failingWriter{}, io.Discard)
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "writing protocol output") {
			t.Fatalf("broken stdout ended with %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("broken stdout did not end the session")
	}
}

// gatedWriter accepts its first write (the ready record) and then holds every
// later write until gate closes, after which it fails like a closed stdout.
type gatedWriter struct {
	mu     sync.Mutex
	writes int
	gate   chan struct{}
}

func (w *gatedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.writes++
	first := w.writes == 1
	w.mu.Unlock()
	if first {
		return len(p), nil
	}
	<-w.gate
	return 0, io.ErrClosedPipe
}

// Stdin ending starts the drain, not a licence to ignore a broken stdout: the
// accepted turn must be cancelled as soon as its output is gone.
func TestJSONProtocolBrokenStdoutDuringEOFDrainCancelsWork(t *testing.T) {
	hold := make(chan struct{})
	defer close(hold)
	p := newProvider(t, hold)
	p.configure(t, "")
	out := &gatedWriter{gate: make(chan struct{})}
	stdin, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serveJSON(context.Background(), openTestAgent(t), t.TempDir(), stdin, out, io.Discard)
	}()
	if _, err := io.WriteString(writer, `{"id":"r1","type":"input","text":"hold the turn"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the turn never reached the model")
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the transport enter the EOF drain
	close(out.gate)
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "writing protocol output") {
			t.Fatalf("broken stdout during the drain ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("broken stdout during the EOF drain did not cancel the accepted turn")
	}
}

// openTestAgent opens a session in the test project directory.
func openTestAgent(t *testing.T) *agent.Agent {
	t.Helper()
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	return a
}
