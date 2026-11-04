package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"dashy/internal/agent"
)

func TestPrintModeEndToEndFakeProvider(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "text/event-stream")
		send := func(v any) {
			data, _ := json.Marshal(v)
			fmt.Fprintf(w, "data: %s\n\n", data)
		}
		if requests == 1 {
			// First response: call bash tool "echo hello from tool"
			args, _ := json.Marshal(map[string]any{"command": "echo hello from tool", "timeout": nil})
			item := map[string]any{
				"type":      "function_call",
				"id":        "call_1",
				"call_id":   "call_1",
				"name":      "bash",
				"arguments": string(args),
			}
			send(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"output": []any{item},
				},
			})
		} else {
			// Second response: the request must carry the bash result for call_1
			var req struct {
				Input []struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
					Output string `json:"output"`
				} `json:"input"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode second request: %v", err)
			}
			found := false
			for _, item := range req.Input {
				if item.Type != "function_call_output" || item.CallID != "call_1" {
					continue
				}
				found = true
				var result struct {
					Output   string `json:"output"`
					ExitCode int    `json:"exit_code"`
				}
				if err := json.Unmarshal([]byte(item.Output), &result); err != nil {
					t.Errorf("tool output is not a result object: %v", err)
				}
				if result.Output != "hello from tool\n" || result.ExitCode != 0 {
					t.Errorf("tool result = %+v, want output %q with exit code 0", result, "hello from tool\n")
				}
			}
			if !found {
				t.Errorf("second request has no function_call_output for call_1")
			}
			// Final assistant answer
			item := map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{
						"type": "output_text",
						"text": "Final answer is 42",
					},
				},
			}
			send(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"output": []any{item},
				},
			})
		}
	}))
	defer server.Close()

	home := setup(t, server.URL)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	err := Run(context.Background(), []string{"-p", "solve this"}, strings.NewReader(""), &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// stdout: final assistant text and nothing else
	if stdout.String() != "Final answer is 42\n" {
		t.Fatalf("stdout mismatch: got %q, want %q", stdout.String(), "Final answer is 42\n")
	}

	// stderr: progress with command, short result, and session id at the end
	errOut := stderr.String()
	if !strings.Contains(errOut, "$ echo hello from tool") {
		t.Fatalf("stderr missing command progress: %q", errOut)
	}
	// The result is its own line; the command line "$ echo hello from tool" does not count.
	resultLine := false
	for _, line := range strings.Split(errOut, "\n") {
		if line == "hello from tool" {
			resultLine = true
		}
	}
	if !resultLine {
		t.Fatalf("stderr missing tool result line: %q", errOut)
	}
	if !strings.Contains(errOut, "session: ") {
		t.Fatalf("stderr missing session: %q", errOut)
	}
	lines := strings.Split(strings.TrimSpace(errOut), "\n")
	lastLine := lines[len(lines)-1]
	if !strings.HasPrefix(lastLine, "session: ") {
		t.Fatalf("expected last line of stderr to be session id, got %q", lastLine)
	}

	// Stderr must NOT contain the final assistant answer
	if strings.Contains(errOut, "Final answer is 42") {
		t.Fatalf("stderr leaked final assistant answer: %q", errOut)
	}

	// -p mode leaves no socket behind once it exits
	runDir := filepath.Join(home, "run")
	if entries, err := os.ReadDir(runDir); err == nil && len(entries) > 0 {
		t.Fatalf("found sockets in %s: %v", runDir, entries)
	}

	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
}

func TestPrintModeResume(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var req struct {
			Input []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "text/event-stream")
		answer := fmt.Sprintf("answer %d", requests)
		if requests == 2 {
			// Second request must have history of first request
			if len(req.Input) < 3 {
				t.Errorf("expected history in resumed session, got %d items", len(req.Input))
			}
		}
		data, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []any{
					map[string]any{
						"type":    "message",
						"role":    "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": answer}},
					},
				},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()

	setup(t, server.URL)

	// Turn 1: create new session
	var out1, diag1 bytes.Buffer
	err := Run(context.Background(), []string{"-p", "turn 1 prompt"}, strings.NewReader(""), &out1, &diag1)
	if err != nil {
		t.Fatal(err)
	}
	if out1.String() != "answer 1\n" {
		t.Fatalf("turn 1 stdout: %q", out1.String())
	}
	sessLine := ""
	for _, l := range strings.Split(diag1.String(), "\n") {
		if strings.HasPrefix(l, "session: ") {
			sessLine = strings.TrimPrefix(l, "session: ")
		}
	}
	if sessLine == "" {
		t.Fatalf("missing session id in turn 1 stderr: %s", diag1.String())
	}

	// Turn 2: resume same session
	var out2, diag2 bytes.Buffer
	err = Run(context.Background(), []string{"-p", "turn 2 prompt", "--resume", sessLine}, strings.NewReader(""), &out2, &diag2)
	if err != nil {
		t.Fatal(err)
	}
	if out2.String() != "answer 2\n" {
		t.Fatalf("turn 2 stdout: %q", out2.String())
	}
	if !strings.Contains(diag2.String(), "session: "+sessLine) {
		t.Fatalf("turn 2 should print resumed session ID %q, got: %s", sessLine, diag2.String())
	}

	// Nonexistent resume ID returns error
	var out3, diag3 bytes.Buffer
	err = Run(context.Background(), []string{"-p", "turn 3", "--resume", "nonexistent-id"}, strings.NewReader(""), &out3, &diag3)
	if err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("expected session not found error, got %v", err)
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}
}

func TestPrintModeSignalsAndExitCodes(t *testing.T) {
	// 1. Success exit code
	if code := ExitCode(nil); code != 0 {
		t.Fatalf("ExitCode(nil) = %d, want 0", code)
	}

	// 2. Setup failure exit code
	setupErr := &ExitError{Code: 1, Err: fmt.Errorf("setup failed")}
	if code := ExitCode(setupErr); code != 1 {
		t.Fatalf("ExitCode(setupErr) = %d, want 1", code)
	}

	// 3. Turn failure: provider 500
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintln(w, "internal server error")
	}))
	defer server.Close()

	setup(t, server.URL)

	var out, diag bytes.Buffer
	err := Run(context.Background(), []string{"-p", "failing turn"}, strings.NewReader(""), &out, &diag)
	if err == nil {
		t.Fatal("expected turn failure, got nil")
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("expected exit code 1 on turn failure, got %d", code)
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on failure, got %q", out.String())
	}
	if !strings.Contains(diag.String(), "session: ") {
		t.Fatalf("expected session id on failure stderr, got %s", diag.String())
	}

	// 4. A signal that arrives before the turn starts also exits 130
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	diag.Reset()
	err = Run(ctx, []string{"-p", "cancelled turn"}, strings.NewReader(""), &out, &diag)
	if code := ExitCode(err); code != 130 {
		t.Fatalf("expected exit code 130 for a cancelled run, got %d (%v)", code, err)
	}

}

func TestBinaryPrintMode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []any{
					map[string]any{
						"type":    "message",
						"role":    "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "binary works"}},
					},
				},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()

	home := setup(t, server.URL)
	bin := build(t)

	runCmd := exec.Command(bin, "-p", "binary test")
	runCmd.Env = append(os.Environ(), "DASHY_HOME="+home)
	var stdout, stderr bytes.Buffer
	runCmd.Stdout = &stdout
	runCmd.Stderr = &stderr
	if err := runCmd.Run(); err != nil {
		t.Fatalf("dashy binary failed: %v, stderr: %s", err, stderr.String())
	}

	if stdout.String() != "binary works\n" {
		t.Fatalf("stdout mismatch: got %q, want %q", stdout.String(), "binary works\n")
	}
	if !strings.Contains(stderr.String(), "session: ") {
		t.Fatalf("stderr missing session: %s", stderr.String())
	}
}

func TestPrintModeRetryOnStderr(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprintln(w, "rate limited")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []any{
					map[string]any{
						"type":    "message",
						"role":    "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "recovered"}},
					},
				},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()

	setup(t, server.URL)

	var out, diag bytes.Buffer
	err := Run(context.Background(), []string{"-p", "retry task"}, strings.NewReader(""), &out, &diag)
	if err != nil {
		t.Fatal(err)
	}

	if out.String() != "recovered\n" {
		t.Fatalf("stdout mismatch: got %q, want %q", out.String(), "recovered\n")
	}

	if !strings.Contains(diag.String(), "Retrying in ") {
		t.Fatalf("expected retry notice on stderr, got: %s", diag.String())
	}
	if !strings.Contains(diag.String(), "session: ") {
		t.Fatalf("expected session id on stderr, got: %s", diag.String())
	}
}

// A sub-agent has its own socket: its shell gets its own DASHY_ENDPOINT, not the parent's,
// and a notification sent to it while it works reaches its model.
func TestPrintModeHasItsOwnNotifySocket(t *testing.T) {
	t.Setenv("DASHY_ENDPOINT", "/tmp/parent-dashy.sock")
	var bodies []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		item := map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "done"}}}
		switch len(bodies) {
		case 1:
			args, _ := json.Marshal(map[string]any{"command": `echo "endpoint=$DASHY_ENDPOINT"`, "timeout": nil})
			item = map[string]any{"type": "function_call", "id": "c1", "call_id": "c1", "name": "bash", "arguments": string(args)}
		case 2:
			// The model's next request carries the endpoint its shell saw; notify it there.
			rest := string(body)[strings.LastIndex(string(body), "endpoint=")+len("endpoint="):]
			endpoint, _, _ := strings.Cut(rest, `\`)
			if endpoint == "" || endpoint == "/tmp/parent-dashy.sock" {
				t.Errorf("sub-agent shell endpoint = %q", endpoint)
			} else if err := agent.Send(endpoint, agent.Message{Text: "late note"}); err != nil {
				t.Errorf("notify the sub-agent: %v", err)
			}
		}
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{"output": []any{item}}})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()
	setup(t, server.URL)
	var stdout, stderr bytes.Buffer
	if err := Run(context.Background(), []string{"-p", "work"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	if len(bodies) != 3 || !strings.Contains(bodies[2], "late note") || !strings.Contains(stderr.String(), "late note") {
		t.Fatalf("notification did not reach the sub-agent: %d requests\n%s", len(bodies), stderr.String())
	}
}
