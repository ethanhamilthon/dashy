package agent

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

// A transient server error is retried; the user sees a notice and the turn succeeds.
func TestRetrySucceedsAfterTransientFailure(t *testing.T) {
	fp := &fakeProvider{
		reply:  func(int, string) string { return textReply("ok") },
		status: func(n int) int { return map[bool]int{true: http.StatusInternalServerError}[n == 1] },
	}
	a := newTestAgent(t, fp, Options{})
	events, err := turn(t, a, "hi")
	if err != nil || len(fp.requests()) != 2 {
		t.Fatalf("err %v after %d requests", err, len(fp.requests()))
	}
	var notice bool
	for _, e := range events {
		notice = notice || (e.Kind == "notice" && strings.HasPrefix(e.Text, "Retrying in"))
	}
	if !notice {
		t.Fatal("retry not shown")
	}
}

// Client errors and persistent server errors are not retried beyond the attempt limit.
func TestRetryStopsWhereItShould(t *testing.T) {
	client := &fakeProvider{reply: func(int, string) string { return textReply("ok") },
		status: func(int) int { return http.StatusBadRequest }}
	if _, err := turn(t, newTestAgent(t, client, Options{}), "hi"); err == nil || len(client.requests()) != 1 {
		t.Fatalf("client error retried: %v after %d", err, len(client.requests()))
	}
	server := &fakeProvider{reply: func(int, string) string { return textReply("ok") },
		status: func(int) int { return http.StatusInternalServerError }}
	if _, err := turn(t, newTestAgent(t, server, Options{}), "hi"); err == nil || len(server.requests()) != retryAttempts {
		t.Fatalf("server errors: %v after %d requests", err, len(server.requests()))
	}
}

// Once output has been shown, a failed stream is never retried, so text is not duplicated.
func TestRetryDoesNotDuplicateStreamedOutput(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.output_text.delta","output_index":0,"delta":"partial"}`)
	}}
	a := newTestAgent(t, fp, Options{})
	if _, err := turn(t, a, "hi"); err == nil || len(fp.requests()) != 1 {
		t.Fatalf("shown output retried: %v after %d requests", err, len(fp.requests()))
	}
}

// The pause doubles from one second and never exceeds eight seconds, even when the
// provider asks for longer (Retry-After is clamped, so huge values cannot overflow).
func TestRetryDelayIsBounded(t *testing.T) {
	if got := delay(0, nil); got != time.Second {
		t.Fatalf("first pause %s", got)
	}
	if got := delay(10, nil); got != retryMax {
		t.Fatalf("capped pause %s", got)
	}
	long := &apiError{Status: 429, RetryAfter: "3600"}
	if got := delay(0, long); got != retryMax {
		t.Fatalf("Retry-After above the cap: %s", got)
	}
	short := &apiError{Status: 429, RetryAfter: "3"}
	if got := delay(0, short); got != 3*time.Second {
		t.Fatalf("Retry-After 3: %s", got)
	}
}
