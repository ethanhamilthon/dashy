package agent

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

const secretKey = "sk-synthetic-secret-7f3a"

// secretAgent serves handler as the provider. The key is either a literal api_key or api_key_env.
func secretAgent(t *testing.T, fromEnv bool, handler http.HandlerFunc) (*Agent, func()) {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	server := httptest.NewServer(handler)
	a, err := New(t.TempDir(), ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	p := Provider{BaseURL: server.URL, APIKey: secretKey}
	if fromEnv {
		t.Setenv("DASHY_TEST_PROVIDER_KEY", secretKey)
		p = Provider{BaseURL: server.URL, APIKeyEnv: "DASHY_TEST_PROVIDER_KEY"}
	}
	a.ApplySettings(Settings{
		Provider:    "p",
		Model:       "m",
		AutoCompact: true,
		Providers:   map[string]Provider{"p": p},
	})
	return a, func() { a.Close(); server.Close() }
}

// assertNoSecret checks every surface an error can reach: texts, records and the saved session.
func assertNoSecret(t *testing.T, a *Agent, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, secretKey) {
			t.Fatalf("secret leaked: %q", text)
		}
	}
	for _, r := range a.Records() {
		if strings.Contains(r.Text, secretKey) {
			t.Fatalf("secret in record: %+v", r)
		}
	}
	data, err := os.ReadFile(a.HistoryPath)
	if os.IsNotExist(err) {
		return // nothing was saved, so nothing could persist
	}
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secretKey)) {
		t.Fatal("secret persisted in session file")
	}
}

func TestProviderFailuresRedactSecret(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"sse failed", http.StatusOK, `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"server_error","message":"upstream rejected ` + secretKey + `"}}}` + "\n\n"},
		{"sse error", http.StatusOK, `data: {"type":"error","code":"invalid_api_key","message":"bad key ` + secretKey + `","param":null}` + "\n\n"},
		{"sse incomplete", http.StatusOK, `data: {"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"` + secretKey + `"}}}` + "\n\n"},
		{"http 401", http.StatusUnauthorized, `{"error":{"message":"Incorrect API key provided: ` + secretKey + `","type":"invalid_request_error","code":"invalid_api_key"}}`},
	} {
		for _, fromEnv := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s env=%v", tc.name, fromEnv), func(t *testing.T) {
				a, closeAll := secretAgent(t, fromEnv, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					if tc.status != http.StatusOK {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(tc.status)
					}
					fmt.Fprint(w, tc.body)
				})
				defer closeAll()
				var events []Event
				err := a.RunInput(context.Background(), Input{Text: "hi"}, func(e Event) { events = append(events, e) })
				if err == nil || !strings.Contains(err.Error(), "[redacted]") {
					t.Fatalf("expected a sanitized failure, got %v", err)
				}
				texts := []string{err.Error()}
				for _, e := range events {
					texts = append(texts, e.Text)
				}
				assertNoSecret(t, a, texts...)
			})
		}
	}
}

func TestCompactErrorsRedactSecret(t *testing.T) {
	a, closeAll := secretAgent(t, true, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":{"message":"summary rejected %s","type":"test"}}`, secretKey)
	})
	defer closeAll()
	a.History = compactHistory(t, 5)
	a.compactKeepTurns = 1
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	_, err := a.Compact(context.Background(), "")
	if err == nil {
		t.Fatal("expected a compaction error")
	}
	assertNoSecret(t, a, err.Error())

	// Automatic compaction reports through an error event instead of returning.
	a.compactThreshold = 100
	var events []Event
	a.compactIfNeeded(context.Background(), func(e Event) { events = append(events, e) })
	if len(events) != 1 {
		t.Fatalf("events = %+v", events)
	}
	assertNoSecret(t, a, events[0].Text)
}

// Literal user and tool content is never scrubbed, even when it looks like a key.
func TestUserContentKeepsSecretText(t *testing.T) {
	a, closeAll := secretAgent(t, false, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		complete(w)
	})
	defer closeAll()
	text := "my key is " + secretKey
	if err := a.Run(context.Background(), text, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range a.Records() {
		found = found || (r.Kind == "user" && r.Text == text)
	}
	if !found {
		t.Fatalf("user text was altered: %+v", a.Records())
	}
}
