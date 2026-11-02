package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCancelledTurnDoesNotCommitRequest(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = a.Run(ctx, "unsent", func(Event) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

func TestSaveFailurePropagatesAfterSuccessfulTurn(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer server.Close()
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	// Point history path to a directory so saving fails
	a.HistoryPath = filepath.Join(t.TempDir(), "dir-history")
	if err := os.MkdirAll(a.HistoryPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := a.Run(context.Background(), "go", func(Event) {}); err == nil || !strings.Contains(err.Error(), "save history") {
		t.Fatalf("save error lost: %v", err)
	}
}

func TestInputDirectlyCommittedWithoutPrefixNotice(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer server.Close()
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})

	var notices []string
	err = a.Run(context.Background(), "hello", func(e Event) {
		if e.Kind == "notice" {
			notices = append(notices, e.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range notices {
		if strings.Contains(n, "Shell session initialized") || strings.Contains(n, "<prefix_output>") {
			t.Fatalf("retired prefix notice found: %s", n)
		}
	}
	if len(a.History) != 1 {
		t.Fatalf("expected exactly 1 user message in history, got %d", len(a.History))
	}
}
