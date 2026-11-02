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
	"syscall"
	"testing"
	"time"
)

type mockTerminalReader struct {
	io.Reader
}

func (m *mockTerminalReader) IsTerminal() bool {
	return true
}

func setupTestHome(t *testing.T, serverURL string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	settings := map[string]any{
		"provider": "test-prov",
		"model":    "test-model",
		"providers": map[string]any{
			"test-prov": map[string]any{
				"base_url": serverURL,
			},
		},
	}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "settings.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return home
}

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
			args, _ := json.Marshal(map[string]any{"command": "echo hello from tool", "timeout": nil, "max_output": nil})
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
			// Second response: final assistant answer
			var req struct {
				Input []json.RawMessage `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
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

	home := setupTestHome(t, server.URL)

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
	if !strings.Contains(errOut, "hello from tool") {
		t.Fatalf("stderr missing tool output: %q", errOut)
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

	// -p mode must start NO socket
	runDir := filepath.Join(home, "run")
	if entries, err := os.ReadDir(runDir); err == nil && len(entries) > 0 {
		t.Fatalf("found sockets in %s: %v", runDir, entries)
	}

	if requests != 2 {
		t.Fatalf("expected 2 requests, got %d", requests)
	}
}

func TestPrintModeStdinAppend(t *testing.T) {
	var receivedInstructions string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for _, item := range req.Input {
			if item.Role == "user" {
				var text string
				if json.Unmarshal(item.Content, &text) == nil {
					receivedInstructions = text
				}
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		data, _ := json.Marshal(map[string]any{
			"type": "response.completed",
			"response": map[string]any{
				"output": []any{
					map[string]any{
						"type":    "message",
						"role":    "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": "ok"}},
					},
				},
			},
		})
		fmt.Fprintf(w, "data: %s\n\n", data)
	}))
	defer server.Close()

	setupTestHome(t, server.URL)

	// Case 1: argument prompt + non-empty stdin -> appended after a blank line
	receivedInstructions = ""
	var out, diag bytes.Buffer
	err := Run(context.Background(), []string{"-p", "main task"}, strings.NewReader("piped details\n"), &out, &diag)
	if err != nil {
		t.Fatal(err)
	}
	if receivedInstructions != "main task\n\npiped details" {
		t.Fatalf("got %q, want %q", receivedInstructions, "main task\n\npiped details")
	}

	// Case 2: empty argument prompt + non-empty stdin -> prompt = stdin
	receivedInstructions = ""
	out.Reset()
	diag.Reset()
	err = Run(context.Background(), []string{"-p", ""}, strings.NewReader("only stdin"), &out, &diag)
	if err != nil {
		t.Fatal(err)
	}
	if receivedInstructions != "only stdin" {
		t.Fatalf("got %q, want %q", receivedInstructions, "only stdin")
	}

	// Case 3: -p without argument + non-empty stdin -> prompt = stdin
	receivedInstructions = ""
	out.Reset()
	diag.Reset()
	err = Run(context.Background(), []string{"-p"}, strings.NewReader("piped without arg"), &out, &diag)
	if err != nil {
		t.Fatal(err)
	}
	if receivedInstructions != "piped without arg" {
		t.Fatalf("got %q, want %q", receivedInstructions, "piped without arg")
	}

	// Case 4: empty prompt + empty stdin -> error (exit code 1)
	out.Reset()
	diag.Reset()
	err = Run(context.Background(), []string{"-p", ""}, strings.NewReader(""), &out, &diag)
	if err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("expected prompt is required error, got %v", err)
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("expected exit code 1, got %d", code)
	}

	// Case 5: terminal stdin is not read
	receivedInstructions = ""
	out.Reset()
	diag.Reset()
	termReader := &mockTerminalReader{Reader: strings.NewReader("ignored stdin")}
	err = Run(context.Background(), []string{"-p", "terminal task"}, termReader, &out, &diag)
	if err != nil {
		t.Fatal(err)
	}
	if receivedInstructions != "terminal task" {
		t.Fatalf("terminal stdin should be ignored, got %q", receivedInstructions)
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

	setupTestHome(t, server.URL)

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

	setupTestHome(t, server.URL)

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

	// 4. Turn cancelled via context (without signal) -> exit code 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out.Reset()
	diag.Reset()
	err = Run(ctx, []string{"-p", "cancelled turn"}, strings.NewReader(""), &out, &diag)
	if err == nil {
		t.Fatal("expected error on cancelled context")
	}
	if code := ExitCode(err); code != 1 {
		t.Fatalf("expected exit code 1 on non-signal cancel, got %d", code)
	}

	// 5. Turn interrupted by SIGINT -> exit code 130
	blockServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(50 * time.Millisecond)
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGINT)
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer blockServer.CloseClientConnections()
	defer blockServer.Close()

	setupTestHome(t, blockServer.URL)
	out.Reset()
	diag.Reset()
	err = Run(context.Background(), []string{"-p", "interrupted turn"}, strings.NewReader(""), &out, &diag)
	if err == nil {
		t.Fatal("expected error on SIGINT, got nil")
	}
	if code := ExitCode(err); code != 130 {
		t.Fatalf("expected exit code 130 on SIGINT, got %d (%v)", code, err)
	}
	if out.Len() != 0 {
		t.Fatalf("expected empty stdout on SIGINT, got %q", out.String())
	}
	if !strings.Contains(diag.String(), "session: ") {
		t.Fatalf("expected session id on SIGINT stderr, got %s", diag.String())
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

	setupTestHome(t, server.URL)

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

	home := setupTestHome(t, server.URL)
	bin := filepath.Join(t.TempDir(), "dashy")

	buildCmd := execCommand("go", "build", "-o", bin, "dashy")
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	runCmd := execCommand(bin, "-p", "binary test")
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

var execCommand = exec.Command
