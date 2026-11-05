package cli

import (
	"bytes"
	"context"
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

	"dashy/internal/agent"
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

// dashy models prints PROVIDER<TAB>MODEL lines from the provider's model list.
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
