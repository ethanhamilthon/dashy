package agent

import (
	"context"
	"strings"
	"testing"

	"dashy/internal/shell"
)

// Steering is accepted after the outputs of the running response, so every call
// keeps its output next to it and the instruction comes after them.
func TestSteeringIsAcceptedAfterToolOutputs(t *testing.T) {
	fp := &fakeProvider{reply: func(n int, body string) string {
		if n == 1 {
			return bashReply("c1", "echo hi")
		}
		return textReply("done")
	}}
	a := newTestAgent(t, fp, Options{})
	steering := &Steering{}
	pushed := false
	emit := func(e Event) {
		if e.Kind == "code" && e.Final && !e.Provisional && !pushed {
			pushed = true
			steering.Push("also check Y")
		}
	}
	if err := a.RunInput(context.Background(), Input{Text: "start"}, emit, steering); err != nil {
		t.Fatal(err)
	}
	second := fp.requests()[1]
	call, output, steer := strings.Index(second, `"function_call"`), strings.Index(second, `"function_call_output"`), strings.Index(second, "also check Y")
	if !(0 <= call && call < output && output < steer) {
		t.Fatalf("order call=%d output=%d steer=%d", call, output, steer)
	}
}

// A queued instruction can be recalled until it is accepted; accepted ones cannot.
func TestSteeringRecallUntilAccepted(t *testing.T) {
	s := &Steering{}
	s.Push("first")
	s.PushInput(Input{Text: "second", Images: []shell.Image{{URL: "data:image/png;base64,AA=="}}})
	if in, ok := s.RecallInput(); !ok || in.Text != "second" || len(in.Images) != 1 {
		t.Fatalf("recalled %+v %v", in, ok)
	}
	if taken := s.TakeInputs(); len(taken) != 1 || taken[0].Text != "first" {
		t.Fatalf("taken %+v", taken)
	}
	if _, ok := s.RecallInput(); ok {
		t.Fatal("accepted instruction recalled")
	}
}
