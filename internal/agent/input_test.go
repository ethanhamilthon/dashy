package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dashy/internal/shell"
)

func TestImageInputAndSteeringPersist(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		body := string(raw(request))
		if !strings.Contains(body, "data:image/png;base64,first") || !strings.Contains(body, "data:image/png;base64,second") {
			t.Error("image bytes missing")
		}
		for _, name := range []string{"first.png", "second.png"} {
			if !strings.Contains(body, "/private/original/"+name) {
				t.Error("original image path missing from model context")
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
	defer server.Close()
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	queue := &Steering{}
	queue.PushInput(Input{Text: "second image", Images: []shell.Image{{URL: "data:image/png;base64,second", Path: "/private/original/second.png"}}})
	input := Input{Images: []shell.Image{{URL: "data:image/png;base64,first", Path: "/private/original/first.png"}}}
	if err = a.RunInput(context.Background(), input, func(Event) {}, queue); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("image-only input failed to complete")
	}
	path := a.HistoryPath
	id := a.SessionInfo()["id"].(string)
	a.Close()
	restored, err := New(cwd, ConfigFiles{}, id)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "base64,first") || !strings.Contains(string(data), "base64,second") {
		t.Fatal("image history not durable")
	}
	if !strings.Contains(fmt.Sprint(restored.Records()), "first.png") {
		t.Fatal("image-only history missing textual label")
	}
}

func TestImageSteeringRecallVersusAcceptance(t *testing.T) {
	for i := 0; i < 100; i++ {
		queue := &Steering{}
		queue.PushInput(Input{Text: "caption", Images: []shell.Image{{URL: "data", Path: "test.png"}}})
		var accepted []Input
		var recalled Input
		var ok bool
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); accepted = queue.TakeInputs() }()
		go func() { defer wg.Done(); recalled, ok = queue.RecallInput() }()
		wg.Wait()
		if ok && len(accepted) != 0 || !ok && len(accepted) != 1 {
			t.Fatal("image instruction duplicated or lost")
		}
		if ok && len(recalled.Images) != 1 || !ok && len(accepted[0].Images) != 1 {
			t.Fatal("attachment not atomic with instruction")
		}
	}
}

func TestInputDisplayDoesNotAlterPlainText(t *testing.T) {
	text := "  literal text\n"
	if (Input{Text: text}).Display() != text {
		t.Fatal("plain message whitespace changed")
	}
	if got := (Input{Text: "look", Images: []shell.Image{{Path: filepath.Join("folder", "photo.png")}}}).Display(); got != "look\n[Image: photo.png]" {
		t.Fatal(got)
	}
}
