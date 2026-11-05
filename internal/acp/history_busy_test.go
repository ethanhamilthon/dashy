package acp

import (
	"sync"
	"testing"
	"time"
)

func TestLoadRejectsBusySessionAndCanRetry(t *testing.T) {
	started, gate := make(chan struct{}), make(chan struct{})
	var release sync.Once
	settings(t, func(string) string {
		close(started)
		<-gate
		return answer("done")
	})
	// Release before server cleanup even if an assertion fails.
	defer release.Do(func() { close(gate) })
	w := connect(t)
	t.Cleanup(func() { release.Do(func() { close(gate) }) })
	w.initialize(t)
	dir := t.TempDir()
	id := w.newSession(t, 2, dir)
	w.prompt(t, 3, id, "work")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt never started")
	}
	if f := load(t, w, 4, id, dir); f.Error == nil || f.Error.Code != -32600 {
		t.Fatalf("loaded busy session: %+v", f)
	}
	release.Do(func() { close(gate) })
	if f := w.response(t, 3); f.Error != nil {
		t.Fatal(f.Error)
	}
	w.updates = nil
	if f := load(t, w, 5, id, dir); f.Error != nil {
		t.Fatal(f.Error)
	}
	if len(w.updates) != 3 {
		t.Fatalf("retry lost history: %s", w.updates)
	}
}
