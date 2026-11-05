package agent

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamFailureDiagnostics(t *testing.T) {
	for _, tc := range []struct{ body, want string }{
		{`{"type":"error"}`, "without details"},
		{`{"type":"response.failed","response":{"error":{}}}`, "without details"},
		{`{"type":"error","error":{"code":"overloaded","message":"try later"}}`, "overloaded try later"},
	} {
		var event streamEvent
		if err := json.Unmarshal([]byte(tc.body), &event); err != nil {
			t.Fatal(err)
		}
		if err := streamFailure(event); !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), event.Type) {
			t.Fatal(err)
		}
	}
}

// A provider error that echoes the API key reaches the user and the events without it.
func TestProviderFailuresNeverShowTheKey(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string {
		return sse(`{"type":"response.failed","response":{"error":{"code":"server_error","message":"bad key sk-secret-123"}}}`)
	}}
	server := httptest.NewServer(fp)
	t.Cleanup(server.Close)
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	s := DefaultSettings()
	s.Provider, s.Model = "p", "m"
	s.Providers["p"] = Provider{BaseURL: server.URL, APIKey: "sk-secret-123"}
	if err := SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}
	a, err := New(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	events, err := turn(t, a, "hi")
	if err == nil || strings.Contains(err.Error(), "sk-secret-123") || !strings.Contains(err.Error(), "bad key") {
		t.Fatalf("error = %v", err)
	}
	for _, e := range events {
		if strings.Contains(e.Text, "sk-secret-123") {
			t.Fatalf("key in event %+v", e)
		}
	}
}
