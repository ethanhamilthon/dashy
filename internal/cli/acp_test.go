package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"net/http/httptest"
)

func TestBinaryACPAndSignal(t *testing.T) {
	provider := httptest.NewServer(&stub{reply: func(int) string { return streamOf("hello") }})
	defer provider.Close()
	home := setup(t, provider.URL)
	bin := build(t)
	for _, signal := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "signal"}[signal], func(t *testing.T) {
			cmd := exec.Command(bin, "acp", "--no-global")
			cmd.Dir = t.TempDir()
			cmd.Env = append(os.Environ(), "DASHY_HOME="+home)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			cmd.Stderr = &diagnostics
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			encoder, decoder := json.NewEncoder(stdin), json.NewDecoder(stdout)
			send := func(id int, method string, params any) {
				t.Helper()
				if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
					t.Fatal(err)
				}
			}
			next := func() map[string]json.RawMessage {
				t.Helper()
				var frame map[string]json.RawMessage
				if err := decoder.Decode(&frame); err != nil {
					t.Fatal(err)
				}
				if string(frame["jsonrpc"]) != `"2.0"` {
					t.Fatal("stdout pollution")
				}
				return frame
			}
			send(1, "initialize", map[string]int{"protocolVersion": 1})
			next()
			send(2, "session/new", map[string]any{"cwd": cmd.Dir, "mcpServers": []any{}})
			var session struct {
				SessionID string `json:"sessionId"`
			}
			f := next()
			for string(f["method"]) == `"session/update"` {
				f = next()
			}
			if err := json.Unmarshal(f["result"], &session); err != nil || session.SessionID == "" {
				t.Fatalf("new session: %s %v", f, err)
			}
			send(3, "session/prompt", map[string]any{"sessionId": session.SessionID, "prompt": []any{map[string]string{"type": "text", "text": "hi"}}})
			messages := 0
			for {
				f := next()
				if string(f["id"]) == "3" {
					break
				}
				if string(f["method"]) == `"session/update"` {
					var notification struct {
						Update struct {
							Kind string `json:"sessionUpdate"`
						}
					}
					if err := json.Unmarshal(f["params"], &notification); err != nil {
						t.Fatal(err)
					}
					if notification.Update.Kind == "agent_message_chunk" {
						messages++
					}
				}
			}
			if messages != 1 {
				t.Fatal("duplicated/missing final message")
			}
			if signal {
				if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
					t.Fatal(err)
				}
			} else {
				stdin.Close()
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			select {
			case err := <-done:
				if signal {
					if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 130 {
						t.Fatalf("signal exit: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("process shutdown hung")
			}
			if strings.Contains(diagnostics.String(), "sk-") || strings.Contains(diagnostics.String(), "\x1b[") {
				t.Fatal("credentials or TUI artifacts in stderr")
			}
		})
	}
}
