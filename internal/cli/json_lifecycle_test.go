package cli

import (
	"context"
	"io"
	"os"
	"sync"
	"testing"
	"time"
)

// releaser returns an idempotent function that closes hold, so a test can
// release held provider requests from both the test body and a cleanup.
func releaser(hold chan struct{}) func() {
	var once sync.Once
	return func() { once.Do(func() { close(hold) }) }
}

// within returns the first output record matching pred that appears before limit, or nil.
func (c *client) within(limit time.Duration, pred func(map[string]any) bool) map[string]any {
	deadline := time.Now().Add(limit)
	for {
		for _, line := range c.snapshot() {
			if pred(line) {
				return line
			}
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// indexOf returns the position of the first output record matching pred, or -1.
func (c *client) indexOf(pred func(map[string]any) bool) int {
	for i, line := range c.snapshot() {
		if pred(line) {
			return i
		}
	}
	return -1
}

func awaitListed(t *testing.T, p *provider, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		select {
		case <-p.listed:
		case <-time.After(10 * time.Second):
			t.Fatalf("model listing %d never reached the provider", i+1)
		}
	}
}

// A /model command is a model listing. It must run in the background like type
// "models", so later requests and stop/steer handling are not blocked by it.
func TestJSONCommandModelDoesNotBlockTheStdinLoop(t *testing.T) {
	hold := make(chan struct{})
	release := releaser(hold)
	defer release()
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"c5","type":"command","name":"/model"}`)
	awaitListed(t, p, 1)
	c.send(`{"id":"s1","type":"state"}`)
	if res := c.within(3*time.Second, resultFor("s1")); res == nil {
		t.Fatal("a /model listing blocked the stdin loop: state waited for the listing")
	}
	release()
	if res := c.await("models", resultFor("c5")); res["ok"] != true {
		t.Fatalf("/model listing = %v", res)
	}
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

// /model counts against the listing cap, so it is refused as busy once eight
// listings are in flight. Before normalization it bypassed that accounting.
func TestJSONCommandModelCountsTowardListingLimit(t *testing.T) {
	hold := make(chan struct{})
	release := releaser(hold)
	defer release()
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	for i := 0; i < maxAsyncRequests; i++ {
		c.send(`{"id":"m` + string(rune('a'+i)) + `","type":"models"}`)
	}
	awaitListed(t, p, maxAsyncRequests)
	c.send(`{"id":"cmd","type":"command","name":"/model"}`)
	res := c.within(3*time.Second, resultFor("cmd"))
	if res == nil {
		t.Fatal("/model did not answer while eight listings were in flight")
	}
	if code := res["error"].(map[string]any)["code"]; code != "busy" {
		t.Fatalf("/model over the listing cap = %v, want busy", res)
	}
	release()
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
}

// Stdin ending must wait for an accepted listing and write its result before the
// shutdown record, instead of closing the transport while the listing runs.
func TestJSONEOFDrainsPendingListing(t *testing.T) {
	hold := make(chan struct{})
	release := releaser(hold)
	defer release()
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"m1","type":"models"}`)
	awaitListed(t, p, 1)
	time.AfterFunc(300*time.Millisecond, release)
	if err := c.finish(); err != nil {
		t.Fatal(err)
	}
	if res := c.within(2*time.Second, resultFor("m1")); res == nil || res["ok"] != true {
		t.Fatalf("listing result missing or failed after EOF: %v", c.snapshot())
	}
	result := c.indexOf(resultFor("m1"))
	shutdown := c.indexOf(typeIs("shutdown"))
	if shutdown < 0 || result > shutdown {
		t.Fatalf("listing result must precede shutdown: %v", c.snapshot())
	}
	if reason := str(c.snapshot()[shutdown], "reason"); reason != "eof" {
		t.Fatalf("shutdown reason = %q", reason)
	}
}

// A signal ends the session, but a pending listing is cancelled and its result
// is written before the shutdown record.
func TestJSONSignalCancelsAndSettlesPendingListing(t *testing.T) {
	hold := make(chan struct{})
	release := releaser(hold)
	defer release()
	p := newProvider(t, hold)
	p.configure(t, "")
	c := startJSON(t)
	c.send(`{"id":"m1","type":"models"}`)
	awaitListed(t, p, 1)
	c.cancel()
	if !c.waitExit(10 * time.Second) {
		t.Fatal("cancelled session did not exit")
	}
	if res := c.within(2*time.Second, resultFor("m1")); res == nil || res["ok"] != false {
		t.Fatalf("cancelled listing must settle with an error result: %v", c.snapshot())
	}
	if c.indexOf(resultFor("m1")) > c.indexOf(typeIs("shutdown")) {
		t.Fatalf("listing result must precede shutdown: %v", c.snapshot())
	}
}

// trackedFile is an os.Pipe read end that reports when a Read returns with an error.
type trackedFile struct {
	*os.File
	once     sync.Once
	returned chan struct{}
}

func (f *trackedFile) Read(p []byte) (int, error) {
	n, err := f.File.Read(p)
	if err != nil {
		f.once.Do(func() { close(f.returned) })
	}
	return n, err
}

// stalledWriter blocks every Write until SetWriteDeadline is called, like a
// stdout nobody reads. returned closes when a Write gives up.
type stalledWriter struct {
	wake       chan struct{}
	wakeOnce   sync.Once
	returned   chan struct{}
	returnOnce sync.Once
}

func newStalledWriter() *stalledWriter {
	return &stalledWriter{wake: make(chan struct{}), returned: make(chan struct{})}
}

func (w *stalledWriter) Write(p []byte) (int, error) {
	<-w.wake
	w.returnOnce.Do(func() { close(w.returned) })
	return 0, os.ErrDeadlineExceeded
}

func (w *stalledWriter) SetWriteDeadline(time.Time) error {
	w.wakeOnce.Do(func() { close(w.wake) })
	return nil
}

// pipeStdin returns the read end of a pipe the test owns; both ends close at cleanup.
func pipeStdin(t *testing.T) *trackedFile {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		w.Close()
		r.Close()
	})
	return &trackedFile{File: r, returned: make(chan struct{})}
}

// Teardown must unblock and join the stdin reader; a read blocked in the OS
// cannot otherwise be interrupted by closing a channel.
func TestJSONTeardownJoinsStdinReader(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	in := pipeStdin(t)
	a := openTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveJSON(ctx, a, t.TempDir(), in, io.Discard, io.Discard) }()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveJSON did not return after cancellation")
	}
	select {
	case <-in.returned:
	default:
		t.Fatal("the stdin reader goroutine outlived serveJSON")
	}
}

// Teardown must unblock and join the stdout writer when it is stalled inside
// Write, instead of abandoning it after the flush grace period.
func TestJSONTeardownJoinsStdoutWriter(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	in := pipeStdin(t)
	out := newStalledWriter()
	t.Cleanup(func() { out.SetWriteDeadline(time.Time{}) })
	a := openTestAgent(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	// The ready record is the first write, so the writer is stalled from the start.
	go func() { done <- serveJSON(ctx, a, t.TempDir(), in, out, io.Discard) }()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serveJSON did not return after cancellation")
	}
	select {
	case <-out.returned:
	default:
		t.Fatal("the stdout writer goroutine outlived serveJSON")
	}
}
