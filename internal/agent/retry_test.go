package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
)

func retryAgent(t *testing.T, handler http.HandlerFunc) (*Agent, *int, func()) {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	}))
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	a.Shell.Dir = cwd
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	return a, &calls, func() { a.Close(); server.Close() }
}

func complete(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
}

func fail(w http.ResponseWriter, status int, retryAfter string) {
	if retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprint(w, `{"error":{"message":"fail","type":"test"}}`)
}

func TestRetrySucceedsAfterTransientFailures(t *testing.T) {
	var seen int
	a, calls, closeAll := retryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		seen++
		if seen < 3 {
			fail(w, http.StatusTooManyRequests, "0")
			return
		}
		complete(w)
	})
	defer closeAll()
	var events []Event
	if err := a.RunInput(context.Background(), Input{Text: "hi"}, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	if *calls != 3 {
		t.Fatalf("calls = %d, want 3", *calls)
	}
	found := false
	for _, e := range events {
		if e.Kind == "notice" && len(e.Text) > 8 && e.Text[:8] == "Retrying" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no retry notice: %+v", events)
	}
}

func TestRetryGivesUpAfterThreeServerErrors(t *testing.T) {
	a, calls, closeAll := retryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusInternalServerError, "")
	})
	defer closeAll()
	err := a.RunInput(context.Background(), Input{Text: "hi"}, func(Event) {})
	if err == nil || *calls != 3 {
		t.Fatalf("err = %v, calls = %d", err, *calls)
	}
}

func TestRetrySkipsClientErrors(t *testing.T) {
	a, calls, closeAll := retryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		fail(w, http.StatusBadRequest, "")
	})
	defer closeAll()
	if err := a.RunInput(context.Background(), Input{Text: "hi"}, func(Event) {}); err == nil {
		t.Fatal("expected error")
	}
	if *calls != 1 {
		t.Fatalf("calls = %d, want 1", *calls)
	}
}

func TestRetryDoesNotDuplicateStreamedOutput(t *testing.T) {
	a, calls, closeAll := retryAgent(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"partial\"}\n\n")
		fail(w, http.StatusInternalServerError, "")
	})
	defer closeAll()
	if err := a.RunInput(context.Background(), Input{Text: "hi"}, func(Event) {}); err == nil {
		t.Fatal("expected error")
	}
	if *calls != 1 {
		t.Fatalf("calls = %d, want 1 after partial output", *calls)
	}
}

func TestRetryableClassification(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{context.Canceled, false},
		{context.DeadlineExceeded, false},
		{&openai.Error{StatusCode: 400}, false},
		{&openai.Error{StatusCode: 429}, true},
		{&openai.Error{StatusCode: 503}, true},
		{&openai.Error{StatusCode: 408}, true},
		{errors.New("write tcp: connection reset by peer"), true},
		{errors.New("provider stream ended without a completed response"), true},
		{errors.New("something else"), false},
	}
	for _, c := range cases {
		if got := retryable(c.err); got != c.want {
			t.Fatalf("retryable(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

func TestDelayHonorsRetryAfter(t *testing.T) {
	header := http.Header{}
	header.Set("Retry-After", "5")
	withHeader := &openai.Error{StatusCode: 429, Response: &http.Response{Header: header}}
	if got := delay(0, withHeader); got < 5*time.Second {
		t.Fatalf("delay = %v, want at least 5s", got)
	}
	if got := delay(0, &openai.Error{StatusCode: 429}); got != time.Second {
		t.Fatalf("delay = %v, want 1s", got)
	}
}
