package control

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

// These tests drive the finish rules on operations that never ran a model
// request, so each operation is finished exactly once.

func TestFinishedTurnPromotesQueuedInputsInOrder(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	var op *operation
	h.c.view(func() {
		op = h.c.startLocked("turn", nil)
		op.steering.PushInput(agent.Input{Text: "next"})
		op.steering.PushInput(agent.Input{Text: "after"})
	})
	h.c.finishTurn(op, nil)
	events := h.await("promoted turn finishes", func(events []Event) bool { return countKind(events, "done") == 2 })
	var started []string
	for _, e := range events {
		if e.Kind == "started" {
			started = append(started, e.Source)
		}
	}
	if len(started) != 1 || started[0] != "queued" {
		t.Fatalf("promotion did not start one queued turn: %v", started)
	}
	if d := doneFor(events, op.id); d == nil || d.Status != "ok" || len(d.Unsent) != 0 {
		t.Fatalf("promoted turn done = %+v", d)
	}
	users := []string{}
	for _, r := range h.c.History() {
		if r.Kind == "user" {
			users = append(users, r.Text)
		}
	}
	if strings.Join(users, ",") != "next,after" {
		t.Fatalf("promoted inputs lost or reordered: %v", users)
	}
}

func TestWithdrawnTurnReturnsOriginalThenQueuedAsUnsent(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	image := shell.Image{URL: "data:image/png;base64,AA==", Path: "original.png"}
	var op *operation
	h.c.view(func() {
		op = h.c.startLocked("turn", []shell.Image{image})
		op.steering.PushInput(agent.Input{Text: "queued"})
	})
	h.c.observe(op, agent.Event{Kind: "withdraw", Text: "original request"})
	h.c.finishTurn(op, errors.New("initialization error"))
	events := h.log()
	d := doneFor(events, op.id)
	if d == nil || d.Status != "error" || d.Error != "initialization error" {
		t.Fatalf("done = %+v", d)
	}
	if len(d.Unsent) != 2 || d.Unsent[0].Text != "original request" || len(d.Unsent[0].Images) != 1 || d.Unsent[1].Text != "queued" {
		t.Fatalf("unsent = %+v", d.Unsent)
	}
	if h.c.Status().Busy || len(h.c.Status().Queued) != 0 {
		t.Fatal("finished turn left state behind")
	}
}

func TestSessionSwitchRefusedWhileImagesAreReturnedUnsent(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	image := shell.Image{URL: "data:image/png;base64,AA==", Path: "kept.png"}
	before := h.a.HistoryPath
	var op *operation
	h.c.view(func() {
		op = h.c.startLocked("turn", []shell.Image{image})
		h.c.deferred = &deferredAction{kind: "new"}
	})
	h.c.observe(op, agent.Event{Kind: "withdraw", Text: "with image"})
	h.c.finishTurn(op, errors.New("boom"))
	events := h.log()
	if !hasError(events, "Submit or remove pending images before switching sessions.") {
		t.Fatalf("switch was not refused: %+v", events)
	}
	if h.a.HistoryPath != before || countKind(events, "session") != 0 {
		t.Fatal("session changed although images would be lost")
	}
	if h.c.deferred != nil {
		t.Fatal("refused switch stayed pending")
	}
}

func hasError(events []Event, text string) bool {
	for _, e := range events {
		if e.Kind == "error" && e.Text == text {
			return true
		}
	}
	return false
}

func TestSessionChangeCompletesAfterUnsentTextOnly(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	first := h.a.HistoryPath
	var op *operation
	h.c.view(func() {
		op = h.c.startLocked("turn", nil)
		op.steering.PushInput(agent.Input{Text: "keep as text"})
		h.c.deferred = &deferredAction{kind: "new"}
	})
	h.c.finishTurn(op, nil)
	if h.a.HistoryPath == first || filepath.Dir(h.a.HistoryPath) != filepath.Dir(first) {
		t.Fatalf("text-only unsent input blocked the switch: %s", h.a.HistoryPath)
	}
	if d := doneFor(h.log(), op.id); d == nil || len(d.Unsent) != 1 {
		t.Fatalf("unsent text not reported: %+v", d)
	}
}

func TestStopMarksActiveOperationCancelling(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	var op *operation
	h.c.view(func() { op = h.c.startLocked("turn", nil) })
	h.c.Stop()
	if !h.c.Status().Cancelling {
		t.Fatalf("status after stop: %+v", h.c.Status())
	}
	h.c.finishTurn(op, nil)
	if h.c.Status().Busy || doneFor(h.log(), op.id).Status != "cancelled" {
		t.Fatal("cancelled operation did not finish as cancelled")
	}
}

func TestNewWhileIdleWaitsForTheNextInput(t *testing.T) {
	h := newHarness(t, &fakeModel{}, "")
	before := h.a.HistoryPath
	if err := h.c.NewSession(); err != nil {
		t.Fatalf("new error: %v", err)
	}
	if h.model.requests != 0 || h.a.HasSavedSession() || h.a.HistoryPath == before {
		t.Fatal("new session started work or wrote a history file before input")
	}
	if !hasSession(h.log()) {
		t.Fatal("session change not reported")
	}
}

func hasSession(events []Event) bool {
	for _, e := range events {
		if e.Kind == "session" {
			return true
		}
	}
	return false
}
