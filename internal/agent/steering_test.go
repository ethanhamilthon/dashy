package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/shell"
)

func TestSteeringIsAcceptedAfterToolOutputs(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	queue := a.Queue()
	pushed, shown := false, false
	emit := func(e Event) {
		if e.Kind == "code" && e.Final && !e.Provisional && !pushed {
			pushed = true
			queue.Push(Input{Text: "also check Y"})
		}
		if e.Kind == "user" && e.Text == "also check Y" {
			shown = true
		}
	}
	if err := a.RunInput(context.Background(), Input{Text: "start"}, emit); err != nil {
		t.Fatal(err)
	}
	second := fp.requests()[1]
	call, output, steer := strings.Index(second, `"function_call"`), strings.Index(second, `"function_call_output"`), strings.Index(second, "also check Y")
	if !(0 <= call && call < output && output < steer) {
		t.Fatalf("order call=%d output=%d steer=%d", call, output, steer)
	}
	if !shown {
		t.Fatal("accepted steering was not emitted as a user event")
	}
}

func TestSteeringRecallUntilAccepted(t *testing.T) {
	s := &Steering{}
	s.Push(Input{Text: "first"})
	s.Push(Input{Text: "second", Images: []shell.Image{{URL: "data:image/png;base64,AA=="}}})
	if in, ok := s.Recall(); !ok || in.Text != "second" || len(in.Images) != 1 {
		t.Fatalf("recalled %+v %v", in, ok)
	}
	if taken := s.Take(); len(taken) != 1 || taken[0].Text != "first" {
		t.Fatalf("taken %+v", taken)
	}
	if _, ok := s.Recall(); ok {
		t.Fatal("accepted instruction recalled")
	}
}

func TestTypedInputMovesRunningCallToBackground(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			args := func(command string) string {
				data, _ := json.Marshal(map[string]any{"command": command, "timeout": nil})
				return string(data)
			}
			return sse(fmt.Sprintf(`{"type":"response.completed","response":{"output":[`+
				`{"type":"function_call","call_id":"c1","name":"bash","arguments":%q},`+
				`{"type":"function_call","call_id":"c2","name":"bash","arguments":%q}]}}`,
				args("touch started; echo early; sleep 2; echo late"), args("touch second")))
		}
		return textReply("ok")
	}}
	a := newTestAgent(t, fp, Options{})
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- a.Run(context.Background(), "go", func(Event) {}) }()
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(filepath.Join(a.Shell.Dir, "started")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command never started")
		}
	}
	a.Queue().Push(Input{Text: "STOP-AND-LISTEN"})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > 1800*time.Millisecond {
		t.Fatalf("the turn waited for the command: %s", took)
	}
	requests := fp.requests()
	if len(requests) != 2 || !strings.Contains(requests[1], "STOP-AND-LISTEN") || !strings.Contains(requests[1], "background as task t1") ||
		!strings.Contains(requests[1], "early") || !strings.Contains(requests[1], "Skipped: the user sent a new message") {
		t.Fatalf("second request: %s", requests[len(requests)-1])
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "second")); err == nil {
		t.Fatal("a call after the message ran")
	}
	got := awaitInput(t, a)
	if !strings.Contains(got[0].Text, `id="t1" status="done"`) || !strings.Contains(got[0].Text, "early") || !strings.Contains(got[0].Text, "late") {
		t.Fatalf("task result %q", got[0].Text)
	}
}
