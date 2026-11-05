package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStreamBudgets(t *testing.T) {
	cases := []struct {
		name   string
		line   string
		repeat int
		want   string
	}{
		{"line", "data: " + strings.Repeat("x", maxStreamEvent) + "\n", 1, "token too long"},
		{"multiline event", "data: " + strings.Repeat("x", maxStreamEvent/2) + "\n", 3, "event byte limit"},
		{"response", ":" + strings.Repeat("x", (1<<20)-2) + "\n", 65, "response byte limit"},
		{"event count", "\n", maxStreamEvents + 1, "event count limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for i := 0; i < tc.repeat; i++ {
					if _, err := fmt.Fprint(w, tc.line); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			c := &client{base: server.URL}
			err := c.stream(context.Background(), map[string]any{}, func(streamEvent) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v want %s", err, tc.want)
			}
		})
	}
}
