package control

import (
	"strings"
	"testing"
)

// An accepted <notify> steering is acknowledged like any other steering: it
// leaves the queue, the turn does not replay it, and it does not block the
// acknowledgement of the input queued after it.
func TestAcceptedNotifySteeringLeavesTheQueue(t *testing.T) {
	model := &fakeModel{gate: make(chan struct{}), started: make(chan struct{}, 4)}
	h := newHarness(t, model, "")
	turn := h.submit(Request{Type: "input", Text: "first"}).Turn
	<-model.started
	note := "<notify>\nplease check the logs\n</notify>"
	if resp := h.submit(Request{Type: "input", Text: note}); resp.Status != "queued" {
		t.Fatalf("notify while busy = %+v", resp)
	}
	h.submit(Request{Type: "steer", Text: "focus on errors"})
	close(model.gate)
	h.await("first turn done", func(events []Event) bool { return doneFor(events, turn) != nil })

	// Done is emitted under the controller lock, and any replacement turn is
	// started inside that same lock, so Status is consistent once it returns.
	if st := h.c.Status(); st.Busy {
		t.Fatalf("accepted steering was promoted into a replacement turn (queued=%d)", len(st.Queued))
	} else if len(st.Queued) != 0 {
		t.Fatalf("accepted steering is still queued: %+v", st.Queued)
	}
	events := h.log()
	if n := countKind(events, "started"); n != 1 {
		t.Fatalf("accepted steering was replayed as a new turn: %d turns started", n)
	}
	if d := doneFor(events, turn); d == nil || d.Status != "ok" || len(d.Unsent) != 0 {
		t.Fatalf("done = %+v; accepted steering must not be returned unsent", d)
	}
	var notice, user bool
	for _, e := range events {
		if e.Kind != "agent" || !e.Agent.Steering {
			continue
		}
		notice = notice || (e.Agent.Kind == "notice" && strings.Contains(e.Agent.Text, "please check the logs"))
		user = user || (e.Agent.Kind == "user" && e.Agent.Text == "focus on errors")
	}
	if !notice || !user {
		t.Fatalf("both steering inputs must be accepted: notice=%v user=%v", notice, user)
	}
	if model.requests != 2 {
		t.Fatalf("accepted steering must trigger one follow-up request, got %d", model.requests)
	}
}
