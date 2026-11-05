package acp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func TestMalformedFramesAndNotifications(t *testing.T) {
	w := connect(t)
	for _, tc := range []struct {
		body string
		code int
	}{
		{`{`, -32700}, {`[]`, -32600}, {`null`, -32600},
		{`{"jsonrpc":"2.0","method":5,"id":1}`, -32600},
		{`{"jsonrpc":"2.0","method":"initialize","id":null}`, -32600},
		{`{"jsonrpc":"2.0","method":"initialize","id":true}`, -32600},
		{`{"jsonrpc":"2.0","method":"initialize","id":1.1}`, -32600},
		{`{"jsonrpc":"2.0","method":"initialize","id":1,"params":[]}`, -32602},
	} {
		if _, err := io.WriteString(w.conn, tc.body+"\n"); err != nil {
			t.Fatal(err)
		}
		if f := w.next(t); f.Error == nil || f.Error.Code != tc.code {
			t.Fatalf("%s: %+v", tc.body, f)
		}
	}
	w.send(t, map[string]any{"jsonrpc": "2.0", "method": "unknown"})
	w.send(t, map[string]any{"jsonrpc": "2.0", "id": "unknown", "result": map[string]any{}})
	w.send(t, map[string]any{"jsonrpc": "2.0", "method": "session/new", "params": map[string]any{}})
	w.initialize(t)
}

func TestVersionAndSessionValidation(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	w := connect(t)
	w.request(t, 1, "initialize", map[string]any{"protocolVersion": 2})
	if f := w.response(t, 1); !strings.Contains(string(f.Result), `"protocolVersion":1`) {
		t.Fatal(f)
	}
	w.request(t, 2, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	if f := w.response(t, 2); f.Error == nil || f.Error.Code != -32600 {
		t.Fatal(f)
	}
	w.initialize(t)
	for _, params := range []any{
		map[string]any{"cwd": "relative", "mcpServers": []any{}},
		map[string]any{"cwd": t.TempDir()},
		map[string]any{"cwd": t.TempDir(), "mcpServers": []any{map[string]string{"name": "unsupported"}}},
		map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}, "additionalDirectories": []string{t.TempDir()}},
	} {
		w.request(t, 3, "session/new", params)
		if f := w.response(t, 3); f.Error == nil || f.Error.Code != -32602 {
			t.Fatal(f)
		}
	}
	w.request(t, 4, "session/new", map[string]any{"cwd": t.TempDir(), "mcpServers": []any{}})
	if f := w.response(t, 4); f.Error == nil || f.Error.Code != -32000 {
		t.Fatal(f)
	}
}

func TestTransportFragmentedAndLargeFrames(t *testing.T) {
	w := connect(t)
	line := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1}}` + "\n"
	for _, b := range []byte(line) {
		if _, err := w.conn.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if f := w.response(t, 1); f.Error != nil {
		t.Fatal(f)
	}
	w.request(t, 2, "unknown", map[string]any{"future": strings.Repeat("x", 128<<10)})
	if f := w.response(t, 2); f.Error == nil || f.Error.Code != -32601 {
		t.Fatal(f)
	}
}

func TestDuplicateRequestIDsDisconnect(t *testing.T) {
	settings(t, func(string) string { return command("sleep 60") })
	w := connect(t)
	w.initialize(t)
	session := w.newSession(t, 2, t.TempDir())
	w.prompt(t, 3, session, "work")
	w.prompt(t, 3, session, "duplicate")
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
		t.Fatal("duplicate ID did not shut down")
	}
}

func TestFrameLimitAndTruncatedFinalFrame(t *testing.T) {
	client, server := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), server, server, Options{}) }()
	go func() { _, _ = io.WriteString(client, strings.Repeat("x", maxFrame+1)) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("oversized frame accepted")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("frame limit hung")
	}
	client.Close()
	in, out := io.Pipe()
	writer, reader := net.Pipe()
	p := newPeer(context.Background(), in, writer)
	seen := make(chan struct{}, 1)
	finished := make(chan error, 1)
	go func() {
		finished <- p.run(func(_ context.Context, _ string, _ json.RawMessage, reply replyFunc) {
			seen <- struct{}{}
			reply(map[string]any{}, nil)
		})
	}()
	go func() { _, _ = io.WriteString(out, `{"jsonrpc":"2.0","id":1,"method":"test"}`); out.Close() }()
	select {
	case <-seen:
	case <-time.After(time.Second):
		t.Fatal("final frame ignored")
	}
	reader.Close()
	<-finished
}

func TestStalledWriterAndQueueOverflow(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()
	p := newPeer(context.Background(), server, server)
	go p.writeLoop()
	if err := p.send(map[string]any{"first": true}); err != nil {
		t.Fatal(err)
	}
	overflow := false
	for i := 0; i < maxPending+2; i++ {
		if p.send(map[string]any{"n": i}) != nil {
			overflow = true
			break
		}
	}
	if !overflow {
		t.Fatal("unbounded queue")
	}
	select {
	case <-p.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("overflow did not cancel")
	}
	client2, server2 := net.Pipe()
	defer client2.Close()
	p2 := newPeer(context.Background(), server2, server2)
	go p2.writeLoop()
	_ = p2.send(map[string]bool{"blocked": true})
	select {
	case <-p2.ctx.Done():
	case <-time.After(writeTimeout + time.Second):
		t.Fatal("stalled writer leaked")
	}
}
