package control

import (
	"testing"

	"dashy/internal/agent"
	"dashy/internal/shell"
)

// startupHarness has a mandatory session_start hook running command; the session is started.
func startupHarness(t *testing.T, command string) (*harness, *fakeModel) {
	t.Helper()
	model := &fakeModel{}
	h := newHarness(t, model, "[hooks.boot]\nevents = ['session_start']\nsend_output = true\ncommand = '"+command+"'\n")
	h.a.Start("test")
	return h, model
}

func modelRequests(model *fakeModel) int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.requests
}

func TestStartupRejectionReturnsInputAndAttachments(t *testing.T) {
	h, model := startupHarness(t, "echo broken; exit 3")
	image := shell.Image{URL: "data:image/png;base64,AA==", Path: "kept.png"}
	file := agent.FileAttachment{Path: "main.go", Content: "package main", Size: 12}
	resp := h.submit(Request{Type: "input", Text: "keep me", Images: []shell.Image{image}, Files: []agent.FileAttachment{file}})
	events := h.await("startup rejection", func(events []Event) bool { return doneFor(events, resp.Turn) != nil })
	d := doneFor(events, resp.Turn)
	if d == nil || d.Status != "error" || len(d.Unsent) != 1 {
		t.Fatalf("done = %+v", d)
	}
	in := d.Unsent[0]
	if in.Text != "keep me" || len(in.Images) != 1 || len(in.Files) != 1 || in.Files[0].Path != "main.go" {
		t.Fatalf("unsent input = %+v", in)
	}
	if n := modelRequests(model); n != 0 {
		t.Fatalf("model requests = %d, want 0", n)
	}
}

func TestStartupCancellationReturnsInputAndAttachments(t *testing.T) {
	h, model := startupHarness(t, "sleep 5")
	image := shell.Image{URL: "data:image/png;base64,AA==", Path: "kept.png"}
	resp := h.submit(Request{Type: "input", Text: "keep me", Images: []shell.Image{image}})
	h.submit(Request{Type: "stop"})
	events := h.await("startup cancellation", func(events []Event) bool { return doneFor(events, resp.Turn) != nil })
	d := doneFor(events, resp.Turn)
	if d == nil || d.Status != "cancelled" || len(d.Unsent) != 1 || d.Unsent[0].Text != "keep me" || len(d.Unsent[0].Images) != 1 {
		t.Fatalf("done = %+v", d)
	}
	if n := modelRequests(model); n != 0 {
		t.Fatalf("model requests = %d, want 0", n)
	}
}
