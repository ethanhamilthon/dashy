package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"dashy/internal/agent"
)

type stub struct {
	mu     sync.Mutex
	bodies []string
	reply  func(n int) string
}

func (s *stub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	data, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, string(data))
	n := len(s.bodies)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, s.reply(n))
}

func streamOf(texts ...string) string {
	var b strings.Builder
	var items []string
	for i, text := range texts {
		id := fmt.Sprintf("m%d", i)
		item := fmt.Sprintf(`{"id":%q,"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}`, id, text)
		items = append(items, item)
		fmt.Fprintf(&b, "data: {\"type\":\"response.output_item.added\",\"output_index\":%d,\"item\":{\"id\":%q,\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n", i, id)
		fmt.Fprintf(&b, "data: {\"type\":\"response.output_text.delta\",\"item_id\":%q,\"output_index\":%d,\"content_index\":0,\"delta\":%q}\n\n", id, i, text)
		fmt.Fprintf(&b, "data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", i, item)
	}
	fmt.Fprintf(&b, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[%s]}}\n\n", strings.Join(items, ","))
	return b.String()
}

func setup(t *testing.T, url string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	s := agent.DefaultSettings()
	s.Provider, s.Model, s.Providers["p"] = "p", "m", agent.Provider{BaseURL: url}
	if err := agent.SaveSettings(home, s); err != nil {
		t.Fatal(err)
	}
	return home
}

func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, diag bytes.Buffer
	err = Run(context.Background(), args, strings.NewReader(stdin), &out, &diag)
	return out.String(), diag.String(), err
}

func TestCommandGrammar(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	bad := [][]string{
		{"version", "garbage"},
		{"help", "garbage"},
		{"-s", "ignored", "-p", "test"},
		{"notify", "--config", "x", "text"},
		{"view", "--no-global", "a.png"},
		{"--no-global"},
		{"acp", "--no-global"},
		{"positional"},
		{"-p", "task", "extra"},
		{"--resume", "x", "-p", "a", "b"},
		{"view"},
	}
	for _, args := range bad {
		if _, _, err := runCLI(t, "", args...); err == nil {
			t.Errorf("accepted %v", args)
		}
	}
	stdout, _, err := runCLI(t, "", "--help")
	if err != nil || !strings.Contains(stdout, "DASHY_HOME") {
		t.Fatalf("help: %v", err)
	}
	if out, _, err := runCLI(t, "", "version"); err != nil || strings.TrimSpace(out) == "" {
		t.Fatalf("version: %q %v", out, err)
	}
}

func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dashy")
	if out, err := exec.Command("go", "build", "-o", bin, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	if err := exec.Command(bin, "version").Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestTUIRefusesNonTerminalStdin(t *testing.T) {
	home := setup(t, "http://127.0.0.1:1")
	if _, _, err := runCLI(t, ""); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "projects")); len(entries) != 0 {
		t.Fatalf("a session was created: %v", entries)
	}
}

func TestModelsListsExactIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"object":"list","data":[{"id":"b-model","object":"model"},{"id":"a/model","object":"model"}]}`)
	}))
	defer srv.Close()
	setup(t, srv.URL)
	stdout, stderr, err := runCLI(t, "", "models")
	if err != nil || stdout != "p\ta/model\np\tb-model\n" {
		t.Fatalf("stdout %q, stderr %q, err %v", stdout, stderr, err)
	}
	if _, _, err := runCLI(t, "", "models", "--provider", "missing"); err == nil {
		t.Fatal("accepted an unknown provider")
	}
	setup(t, "http://127.0.0.1:1")
	if _, stderr, err := runCLI(t, "", "models"); ExitCode(err) != 1 || !strings.HasPrefix(stderr, "p: ") {
		t.Fatalf("stderr %q, err %v", stderr, err)
	}
}
