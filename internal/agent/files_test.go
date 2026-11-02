package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"dashy/internal/shell"
)

func TestTextFileReadBounds(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int
		content    string
		truncated  bool
		size       int64
	}{
		{"exact limit", "12345678", 8, "12345678", false, 8},
		{"limit plus one", "123456789", 8, "12345678", true, 9},
		{"unlimited", strings.Repeat("x", 100), 0, strings.Repeat("x", 100), false, 100},
		{"empty", "", 8, "", false, 0},
		{"utf8 cut mid rune", "aé", 2, "a", true, 3},
		{"utf8 complete at limit", "aé", 3, "aé", false, 3},
		{"utf8 cut first byte", "é", 1, "", true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "f.txt"), []byte(tc.body), 0600); err != nil {
				t.Fatal(err)
			}
			files, err := ReadTextFiles(root, []string{"f.txt"}, tc.limit)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 1 {
				t.Fatalf("got %d attachments", len(files))
			}
			got := files[0]
			if got.Content != tc.content || got.Truncated != tc.truncated || got.Size != tc.size || got.Path != "f.txt" {
				t.Fatalf("got %+v", got)
			}
		})
	}
}

func TestTextFileRejectsInvalidContent(t *testing.T) {
	for name, body := range map[string]string{
		"invalid utf8":          "a\xffb",
		"nul byte":              "a\x00b",
		"invalid inside prefix": "a\xff" + strings.Repeat("b", 6),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "bad.txt"), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadTextFiles(root, []string{"bad.txt"}, 3); err == nil {
				t.Fatal("binary or invalid text accepted")
			}
		})
	}
}

func TestTextFileMissingAndSpecialFiles(t *testing.T) {
	root := t.TempDir()
	if _, err := ReadTextFiles(root, []string{"missing.txt"}, 8); err == nil {
		t.Fatal("missing file did not report an error")
	}
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	if _, err := ReadTextFiles(root, []string{"pipe"}, 8); err == nil {
		t.Fatal("special file accepted")
	}
}

func TestTextFileUnreadableReportsError(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "locked.txt")
	if err := os.WriteFile(path, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)
	if f, err := os.Open(path); err == nil {
		f.Close()
		t.Skip("file remains readable (likely root); permission check not meaningful")
	}
	if _, err := ReadTextFiles(root, []string{"locked.txt"}, 8); err == nil {
		t.Fatal("unreadable file did not report an error")
	}
}

func TestTextFileRootContainment(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside.txt")
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "inside.txt"), []byte("inside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "inside.txt"), filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{outside, "../outside.txt", "escape.txt"} {
		if _, err := ReadTextFiles(root, []string{p}, 64); err == nil {
			t.Fatalf("path %q escaped the project root", p)
		}
	}
	files, err := ReadTextFiles(root, []string{"alias.txt"}, 64)
	if err != nil || len(files) != 1 || files[0].Content != "inside" {
		t.Fatalf("inside symlink not read: %+v %v", files, err)
	}
}

// The number of files per message is bounded: each one may be up to the byte
// limit, so an unbounded list could load arbitrarily much memory from one request.
func TestTextFileCountIsBounded(t *testing.T) {
	root := t.TempDir()
	var paths []string
	for i := 0; i <= maxFilesPerInput; i++ {
		name := fmt.Sprintf("f%03d.txt", i)
		if err := os.WriteFile(filepath.Join(root, name), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, name)
	}
	if _, err := ReadTextFiles(root, paths, 0); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("%d files were accepted: %v", len(paths), err)
	}
	files, err := ReadTextFiles(root, paths[:maxFilesPerInput], 0)
	if err != nil || len(files) != maxFilesPerInput {
		t.Fatalf("the limit itself must be accepted: %d files, %v", len(files), err)
	}
}

func TestTextFileDedupeOrderAndDirectories(t *testing.T) {
	root := t.TempDir()
	for name, body := range map[string]string{"a.txt": "A", "b.txt": "B"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(root, "dir"), 0700); err != nil {
		t.Fatal(err)
	}
	files, err := ReadTextFiles(root, []string{"b.txt", "./a.txt", "dir", "sub/../b.txt", "a.txt"}, 64)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	if !slices.Equal(paths, []string{"b.txt", "a.txt"}) {
		t.Fatalf("paths = %q", paths)
	}
}

// fakeModel records each Responses request body and answers with an empty turn.
func fakeModel(bodies *[][]byte) *httptest.Server {
	var mu sync.Mutex
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		*bodies = append(*bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"output\":[]}}\n\n")
	}))
}

// requestUserTexts returns the text of each user message in a request body.
func requestUserTexts(t *testing.T, body []byte) []string {
	t.Helper()
	var payload struct {
		Input []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, item := range payload.Input {
		if item.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(item.Content, &text) == nil {
			texts = append(texts, text)
			continue
		}
		var parts []struct{ Type, Text string }
		if err := json.Unmarshal(item.Content, &parts); err != nil {
			t.Fatal(err)
		}
		var inputs []string
		for _, part := range parts {
			if part.Type == "input_text" {
				inputs = append(inputs, part.Text)
			}
		}
		texts = append(texts, strings.Join(inputs, "\n"))
	}
	return texts
}

func newFileTestAgent(t *testing.T, cwd, url string, maxFileBytes int) *Agent {
	t.Helper()
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	configureTestAgent(t, a, Settings{Provider: "p", Model: "m", MaxFileBytes: maxFileBytes, Providers: map[string]Provider{"p": {BaseURL: url}}})
	t.Cleanup(func() { a.Close() })
	return a
}

func TestFileAttachmentReachesModelInSameMessage(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "main.go"), []byte("package main // </file> & <tag>\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	server := fakeModel(&bodies)
	defer server.Close()
	a := newFileTestAgent(t, cwd, server.URL, 64)
	files, err := ReadTextFiles(cwd, []string{"main.go", "./main.go"}, 64)
	if err != nil {
		t.Fatal(err)
	}
	input := Input{Text: "explain @main.go", Files: files}
	var events []Event
	if err := a.RunInput(context.Background(), input, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatal(err)
	}
	texts := requestUserTexts(t, bodies[0])
	if len(texts) != 1 {
		t.Fatalf("user messages = %d, want 1", len(texts))
	}
	message := texts[0]
	if !strings.HasPrefix(message, "explain @main.go") || !strings.Contains(message, `path="main.go"`) {
		t.Fatalf("prompt or path missing: %q", message)
	}
	if !strings.Contains(message, "package main // &lt;/file&gt; &amp; &lt;tag&gt;") {
		t.Fatalf("content not escaped: %q", message)
	}
	if strings.Count(message, "package main") != 1 {
		t.Fatalf("duplicate reference attached twice: %q", message)
	}
	for _, e := range events {
		if strings.Contains(e.Text, "package main") {
			t.Fatalf("file body in live event %q: %q", e.Kind, e.Text)
		}
	}
	if strings.Contains(input.Display(), "package main") {
		t.Fatal("file body in Display")
	}
}

func TestFileAttachmentWithImagesAndTruncation(t *testing.T) {
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "big.txt"), []byte("start "+strings.Repeat("x", 100)+" END"), 0600); err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	server := fakeModel(&bodies)
	defer server.Close()
	a := newFileTestAgent(t, cwd, server.URL, 16)
	files, err := ReadTextFiles(cwd, []string{"big.txt"}, 16)
	if err != nil {
		t.Fatal(err)
	}
	image := shell.Image{URL: "data:image/png;base64,AA==", Path: "/private/original/pic.png"}
	if err := a.RunInput(context.Background(), Input{Text: "use @big.txt", Images: []shell.Image{image}, Files: files}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	body := string(bodies[0])
	if !strings.Contains(body, "data:image/png;base64,AA==") {
		t.Fatal("image dropped when files attached")
	}
	texts := requestUserTexts(t, bodies[0])
	if len(texts) != 1 || !strings.Contains(texts[0], "start xxxxxxxxxx") || strings.Contains(texts[0], "END") {
		t.Fatalf("truncated prefix wrong: %q", texts)
	}
	if !strings.Contains(texts[0], `truncated="true"`) || !strings.Contains(texts[0], "[truncated: showing the first 16 of 110 bytes]") {
		t.Fatalf("truncation not marked: %q", texts[0])
	}
}

func TestFileAttachmentSteeringKeepsSubmittedSnapshot(t *testing.T) {
	cwd := t.TempDir()
	notes := filepath.Join(cwd, "notes.txt")
	if err := os.WriteFile(notes, []byte("version one"), 0600); err != nil {
		t.Fatal(err)
	}
	files, err := ReadTextFiles(cwd, []string{"notes.txt"}, 64)
	if err != nil {
		t.Fatal(err)
	}
	queue := &Steering{}
	queue.PushInput(Input{Text: "follow @notes.txt", Files: files})
	if err := os.WriteFile(notes, []byte("version two"), 0600); err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	server := fakeModel(&bodies)
	defer server.Close()
	a := newFileTestAgent(t, cwd, server.URL, 64)
	if err := a.RunInput(context.Background(), Input{Text: "start"}, func(Event) {}, queue); err != nil {
		t.Fatal(err)
	}
	texts := requestUserTexts(t, bodies[0])
	joined := strings.Join(texts, "\n")
	if !strings.Contains(joined, "version one") || strings.Contains(joined, "version two") {
		t.Fatalf("steering re-read the file: %q", joined)
	}
}

func TestFileAttachmentSnapshotSurvivesSourceChangesAndReopen(t *testing.T) {
	cwd := t.TempDir()
	source := filepath.Join(cwd, "big.txt")
	if err := os.WriteFile(source, []byte("start "+strings.Repeat("x", 100)+" END"), 0600); err != nil {
		t.Fatal(err)
	}
	var bodies [][]byte
	server := fakeModel(&bodies)
	defer server.Close()
	a := newFileTestAgent(t, cwd, server.URL, 16)
	files, err := ReadTextFiles(cwd, []string{"big.txt"}, 16)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.RunInput(context.Background(), Input{Text: "see @big.txt", Files: files}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	id := a.SessionInfo()["id"].(string)
	home := a.home
	a.Close()
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	restored, err := New(cwd, ConfigFiles{}, id)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	configureTestAgent(t, restored, Settings{Provider: "p", Model: "m", MaxFileBytes: 16, Providers: map[string]Provider{"p": {BaseURL: server.URL}}})
	for _, r := range restored.Records() {
		if strings.Contains(r.Text, "xxxx") || strings.Contains(r.Text, "<file") {
			t.Fatalf("restored display contains file body: %+v", r)
		}
	}
	found := false
	for _, r := range restored.Records() {
		if r.Kind == "user" && r.Text == "see @big.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("compact prompt missing from restored display: %+v", restored.Records())
	}
	if err := restored.RunInput(context.Background(), Input{Text: "again"}, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	texts := requestUserTexts(t, bodies[len(bodies)-1])
	if !strings.Contains(strings.Join(texts, "\n"), "start xxxxxxxxxx") {
		t.Fatalf("reopened model history lost the snapshot: %q (home %s)", texts, home)
	}
}
