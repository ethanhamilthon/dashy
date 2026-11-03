package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// stub is a provider that answers with a full Responses stream; each request is recorded.
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

// streamOf is a realistic stream: an item per message (added, delta, done), then response.completed.
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

// setup writes settings for a provider at url into a fresh DASHY_HOME and returns it.
func setup(t *testing.T, url string) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	settings := map[string]any{"provider": "p", "model": "m", "providers": map[string]any{"p": map[string]any{"base_url": url}}}
	data, _ := json.Marshal(settings)
	if err := os.WriteFile(filepath.Join(home, "settings.json"), data, 0600); err != nil {
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

// The one-shot answer is the committed final text, exactly once: never the streamed deltas or the item-done event as well.
func TestPrintStdoutIsTheCommittedAnswerOnce(t *testing.T) {
	s := &stub{reply: func(int) string { return streamOf("UNIQUE FINAL") }}
	srv := httptest.NewServer(s)
	defer srv.Close()
	setup(t, srv.URL)
	stdout, stderr, err := runCLI(t, "", "-p", "say it")
	if err != nil || stdout != "UNIQUE FINAL\n" {
		t.Fatalf("stdout %q, err %v", stdout, err)
	}
	if !strings.Contains(stderr, "session: ") {
		t.Fatalf("stderr lacks the session line: %q", stderr)
	}
}

func TestPrintKeepsEveryFinalMessageItem(t *testing.T) {
	srv := httptest.NewServer(&stub{reply: func(int) string { return streamOf("first part", "second part") }})
	defer srv.Close()
	setup(t, srv.URL)
	stdout, _, err := runCLI(t, "", "-p", "two items")
	if err != nil || stdout != "first part\nsecond part\n" {
		t.Fatalf("stdout %q, err %v", stdout, err)
	}
}

// Stdin is appended after a blank line; a prompt may be empty only when stdin has text.
func TestPrintAppendsStdin(t *testing.T) {
	s := &stub{reply: func(int) string { return streamOf("ok") }}
	srv := httptest.NewServer(s)
	defer srv.Close()
	setup(t, srv.URL)
	if _, _, err := runCLI(t, "from stdin\n", "-p", "task"); err != nil {
		t.Fatal(err)
	}
	if body := s.bodies[0]; !strings.Contains(body, `task\n\nfrom stdin`) {
		t.Fatalf("stdin not appended: %s", body)
	}
	if _, _, err := runCLI(t, "", "-p="); err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("empty prompt: %v", err)
	}
}

// The value of -p is the prompt literally, even when it starts with a hyphen or is itself "-p".
func TestPrintTakesHyphenPromptLiterally(t *testing.T) {
	s := &stub{reply: func(int) string { return streamOf("ok") }}
	srv := httptest.NewServer(s)
	defer srv.Close()
	setup(t, srv.URL)
	for i, prompt := range []string{"-explain this", "--help", "-p"} {
		if _, _, err := runCLI(t, "", "-p", prompt); err != nil {
			t.Fatalf("%q: %v", prompt, err)
		}
		if !strings.Contains(s.bodies[i], prompt) {
			t.Fatalf("prompt %q not sent: %s", prompt, s.bodies[i])
		}
	}
}

func TestPrintFailureExitsWithErrorAndNoStdout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"nope"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	setup(t, srv.URL)
	stdout, _, err := runCLI(t, "", "-p", "fail")
	if err == nil || ExitCode(err) != 1 || stdout != "" {
		t.Fatalf("failure: stdout %q, code %d, err %v", stdout, ExitCode(err), err)
	}
}

func TestPrintWithoutModelIsASetupError(t *testing.T) {
	setup(t, "http://127.0.0.1:1")
	t.Setenv("DASHY_HOME", t.TempDir())
	if _, _, err := runCLI(t, "", "-p", "x"); err == nil || !strings.Contains(err.Error(), "no provider or model") {
		t.Fatalf("err = %v", err)
	}
}

// The command grammar: each subcommand accepts only its own flags and positionals.
func TestCommandGrammar(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	bad := [][]string{
		{"version", "garbage"},
		{"help", "garbage"},
		{"-s", "ignored", "-p", "test"},
		{"notify", "--config", "x", "text"},
		{"view", "--no-global", "a.png"},
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

// Flag-like literals and DASHY_* names in the sources of this module.
var (
	dashedFlag   = regexp.MustCompile(`^--?[a-z][a-z-]*=?$`)
	variableName = regexp.MustCompile(`DASHY_[A-Z][A-Z_]*`)
)

// sourceFlagsAndVariables reads the non-test Go sources of the module. Flags are the
// names given to flag definitions in this package (StringVar, BoolVar, Var, Func) and
// the dashed flag literals of this package (such as "--help"); variables are the
// DASHY_* names in any string literal. Nothing here is maintained by hand.
func sourceFlagsAndVariables(t *testing.T) (flags, variables map[string]bool) {
	t.Helper()
	flags, variables = map[string]bool{}, map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir("../..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != "../.." && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		inCLI := filepath.Base(filepath.Dir(path)) == "cli"
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.BasicLit:
				value, err := strconv.Unquote(n.Value)
				if n.Kind != token.STRING || err != nil {
					return true
				}
				for _, v := range variableName.FindAllString(value, -1) {
					variables[v] = true
				}
				if inCLI && dashedFlag.MatchString(value) {
					flags[strings.TrimLeft(strings.TrimSuffix(value, "="), "-")] = true
				}
			case *ast.CallExpr:
				sel, ok := n.Fun.(*ast.SelectorExpr)
				if !inCLI || !ok || len(n.Args) < 2 {
					return true
				}
				index := 1
				if sel.Sel.Name == "Func" {
					index = 0
				} else if !strings.HasSuffix(sel.Sel.Name, "Var") {
					return true
				}
				if lit, ok := n.Args[index].(*ast.BasicLit); ok && lit.Kind == token.STRING {
					if name, err := strconv.Unquote(lit.Value); err == nil {
						flags[name] = true
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return flags, variables
}

// The help text names every flag the code defines and every DASHY_* variable the code uses.
// Both sets come from the sources, so an undocumented addition fails this test.
func TestHelpNamesEveryFlagAndVariable(t *testing.T) {
	flags, variables := sourceFlagsAndVariables(t)
	if len(flags) < 8 || len(variables) < 5 {
		t.Fatalf("source scan found %d flags and %d variables; the scanner is broken", len(flags), len(variables))
	}
	for name := range flags {
		flag := "--" + name
		if len(name) == 1 {
			flag = "-" + name
		}
		if !strings.Contains(helpText, flag) {
			t.Errorf("help does not mention flag %s", flag)
		}
	}
	for name := range variables {
		if !strings.Contains(helpText, name) {
			t.Errorf("help does not mention %s", name)
		}
	}
}

// build compiles the dashy binary once for the process-level tests.
func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "dashy")
	if out, err := exec.Command("go", "build", "-o", bin, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	// The first run of a fresh binary can be slow (macOS checks new executables);
	// running it once here keeps the signal tests from signalling a process that is still starting.
	if err := exec.Command(bin, "version").Run(); err != nil {
		t.Fatal(err)
	}
	return bin
}

// start launches the binary in dir with DASHY_HOME=home.
func start(t *testing.T, bin, dir, home string, stdin io.Reader, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DASHY_HOME="+home)
	cmd.Stdin = stdin
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// waitExit waits until the process is in the state wait describes, sends sig and
// returns the exit code and how long the process took to exit.
func waitExit(t *testing.T, cmd *exec.Cmd, sig syscall.Signal, wait func()) (int, time.Duration) {
	t.Helper()
	wait()
	start := time.Now()
	cmd.Process.Signal(sig)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), time.Since(start)
		}
		return 0, time.Since(start)
	case <-time.After(15 * time.Second):
		cmd.Process.Kill()
		t.Fatalf("process did not exit after %v", sig)
	}
	return -1, 0
}

// A signal while the one-shot run waits for stdin (an open pipe) exits 130 at once.
func TestSignalWhileReadingStdinExits130(t *testing.T) {
	bin := build(t)
	home := setup(t, "http://127.0.0.1:1")
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd := start(t, bin, t.TempDir(), home, reader, "-p", "x")
		// Give the child time to reach main and install its signal handling before signalling.
		code, took := waitExit(t, cmd, sig, func() { time.Sleep(time.Second) })
		writer.Close()
		reader.Close()
		if code != 130 || took > 5*time.Second {
			t.Fatalf("%v: exit %d after %s", sig, code, took)
		}
	}
}

// A signal while a dashy block runs cancels the block (its process group) and exits 130.
func TestSignalDuringDashyBlockExits130(t *testing.T) {
	bin := build(t)
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		project := t.TempDir()
		marker := filepath.Join(project, "started")
		if err := os.WriteFile(filepath.Join(project, "AGENTS.md"), []byte("```dashy\ntouch started; sleep 60\n```\n"), 0600); err != nil {
			t.Fatal(err)
		}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		}))
		home := setup(t, srv.URL)
		cmd := start(t, bin, project, home, strings.NewReader(""), "-p", "x")
		code, took := waitExit(t, cmd, sig, func() {
			deadline := time.Now().Add(10 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					return
				}
				if time.Now().After(deadline) {
					t.Fatal("the dashy block never started")
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
		srv.Close()
		if code != 130 || took > 10*time.Second {
			t.Fatalf("%v during a dashy block: exit %d after %s", sig, code, took)
		}
	}
}

// A bare -p (no value) takes its prompt from stdin; with empty stdin there is no prompt at all.
func TestPrintBareFlagTakesPromptFromStdin(t *testing.T) {
	s := &stub{reply: func(int) string { return streamOf("ok") }}
	srv := httptest.NewServer(s)
	defer srv.Close()
	setup(t, srv.URL)
	if _, _, err := runCLI(t, "from stdin\n", "-p"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s.bodies[0], "from stdin") {
		t.Fatalf("stdin not sent as the prompt: %s", s.bodies[0])
	}
	if _, _, err := runCLI(t, "", "-p"); err == nil || !strings.Contains(err.Error(), "prompt is required") {
		t.Fatalf("bare -p with empty stdin: %v", err)
	}
}

// The interactive TUI needs a terminal; piped input is refused before any session is created.
func TestTUIRefusesNonTerminalStdin(t *testing.T) {
	home := setup(t, "http://127.0.0.1:1")
	if _, _, err := runCLI(t, "", "--no-global"); err == nil || !strings.Contains(err.Error(), "needs a terminal") {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "projects")); len(entries) != 0 {
		t.Fatalf("a session was created: %v", entries)
	}
}
