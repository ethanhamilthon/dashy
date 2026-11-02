package hooks

import (
	"context"
	"testing"
	"time"
)

func TestStartupFailsIfRequiredOutputCannotBeDelivered(t *testing.T) {
	r := &Runner{}
	for i := 0; i < maxPending; i++ {
		r.recordOutput(Result{SessionID: "session", Event: "turn_end", Output: "old output"})
	}
	r.Set([]Hook{{Name: "context", Events: []string{"session_start"}, SendOutput: true, Command: "echo required-context"}})
	defer r.Wait(time.Second)
	r.Fire("session_start", map[string]string{"DASHY_SESSION_ID": "session"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := r.WaitStartup(ctx, "session"); err == nil {
		t.Fatal("startup succeeded even though its required output was dropped")
	}
}

func TestSupersededBootstrapCannotDeliverLateOutput(t *testing.T) {
	old, current := &startup{}, &startup{}
	r := &Runner{startups: map[string]*startup{"session": current}}
	if err := r.recordOutput(Result{bootstrap: old, SessionID: "session", Event: "session_start", Output: "stale-context"}); err == nil {
		t.Fatal("superseded bootstrap output accepted")
	}
	if got := r.DrainOutputs("session"); len(got) != 0 {
		t.Fatalf("superseded context was delivered: %+v", got)
	}
	if err := r.recordOutput(Result{bootstrap: current, SessionID: "session", Event: "session_start", Output: "current-context"}); err != nil {
		t.Fatal(err)
	}
	if got := r.DrainOutputs("session"); len(got) != 1 || got[0].Output != "current-context" {
		t.Fatalf("current context not delivered: %+v", got)
	}
}
