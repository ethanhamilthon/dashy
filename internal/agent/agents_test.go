package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// systemPrompt assembles the prompt of a fresh agent with the given global and local AGENTS.md content.
func systemPrompt(t *testing.T, global, local string) string {
	t.Helper()
	a := newTestAgentWith(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{}, func(home, dir string) {
		if global != "" {
			writeFile(t, filepath.Join(home, "AGENTS.md"), global)
		}
		if local != "" {
			writeFile(t, filepath.Join(dir, "AGENTS.md"), local)
		}
	})
	prompt, err := a.instructions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

// Files are added verbatim under "# path" headers, separated by exactly one blank line.
func TestPromptKeepsFileBytesVerbatim(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	global, local := filepath.Join(a.home, "AGENTS.md"), filepath.Join(a.Shell.Dir, "AGENTS.md")
	writeFile(t, global, "g\r\nline")
	writeFile(t, local, "l\n\n\n")
	a.promptFiles = []string{global, local}
	got, err := a.instructions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "# " + global + "\ng\r\nline\n\n# " + local + "\nl\n\n\n"
	if got != want {
		t.Fatalf("prompt = %q\nwant     %q", got, want)
	}
}

func TestNoAGENTSFilesMeansEmptyPrompt(t *testing.T) {
	if got := systemPrompt(t, "", ""); got != "" {
		t.Fatalf("prompt = %q", got)
	}
	a := newTestAgent(t, &fakeProvider{}, Options{NoGlobal: true})
	if len(a.promptFiles) != 0 {
		t.Fatalf("files = %v", a.promptFiles)
	}
}

// body returns the prompt without its "# path" header line.
func body(prompt string) string { return prompt[strings.Index(prompt, "\n")+1:] }

// Only top-level fences with the exact info string "dashy" run. Nested, other
// info strings and quoted fences stay verbatim; the fence lines of a block vanish.
func TestOnlyTopLevelDashyBlocksRun(t *testing.T) {
	src := "before\n```dashy\necho one\n```\n- item\n\n  ```dashy\n  echo nested\n  ```\n```sh\necho sh\n```\n> ```dashy\n> echo quoted\n> ```\nafter\n"
	want := "before\none\n- item\n\n  ```dashy\n  echo nested\n  ```\n```sh\necho sh\n```\n> ```dashy\n> echo quoted\n> ```\nafter\n"
	if got := body(systemPrompt(t, src, "")); got != want {
		t.Fatalf("body = %q\nwant   %q", got, want)
	}
}

// A failing block becomes its failure text with a short stderr tail; blocks run in order.
func TestBlockFailureTextAndSequentialOrder(t *testing.T) {
	src := "```dashy\necho first; echo tail-msg >&2; exit 3\n```\n```dashy\necho second\n```\n"
	want := "[dashy block failed: exit status 3]\ntail-msg\nsecond\n"
	if got := body(systemPrompt(t, src, "")); got != want {
		t.Fatalf("body = %q", got)
	}
}

func TestBlockTimeoutIsAFailure(t *testing.T) {
	old := blockTimeout
	blockTimeout = 300 * time.Millisecond
	t.Cleanup(func() { blockTimeout = old })
	got := systemPrompt(t, "```dashy\nsleep 30\n```\n", "")
	if !strings.Contains(got, "[dashy block failed: timed out after 300ms]") {
		t.Fatalf("prompt = %q", got)
	}
}

// Stdout keeps its prefix within the combined 64 KiB budget; stderr keeps only a short tail.
func TestBlockOutputBudget(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	out, err := a.runBlock(context.Background(), "head -c 200000 /dev/zero | tr '\\0' a", "tui", "startup")
	if err != nil || len(out) != blockOutput-blockStderrTail {
		t.Fatalf("stdout kept %d bytes, err %v", len(out), err)
	}
	failed, err := a.runBlock(context.Background(), "head -c 100000 /dev/zero | tr '\\0' a; echo boom >&2; exit 1", "tui", "startup")
	if err != nil || failed != "[dashy block failed: exit status 1]\nboom" {
		t.Fatalf("failure after the capture limit: %q %v", failed, err)
	}
	many := "for i in $(seq 1 40); do echo line$i >&2; done; exit 2"
	failed, _ = a.runBlock(context.Background(), many, "tui", "startup")
	if !strings.HasSuffix(failed, "line36\nline37\nline38\nline39\nline40") || strings.Count(failed, "\n") > blockTailLines {
		t.Fatalf("stderr tail = %q", failed)
	}
}

// A selected AGENTS.md that cannot be read is an error, never a shorter prompt.
func TestSelectedFileVanishingIsAnError(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, func(_, dir string) {
		writeFile(t, filepath.Join(dir, "AGENTS.md"), "x")
	})
	if err := os.Remove(filepath.Join(a.Shell.Dir, "AGENTS.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.instructions(context.Background()); err == nil {
		t.Fatal("missing selected file was skipped")
	}
}

func TestCancelledTurnStopsWhileBlockRuns(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, func(_, dir string) {
		writeFile(t, filepath.Join(dir, "AGENTS.md"), "```dashy\nsleep 30\n```\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := a.assemblePrompt(ctx); err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("cancel: %v after %s", err, time.Since(start))
	}
}

// /resume of the current session assembles the prompt again: changed files and
// blocks take effect once, then the new prompt is frozen for the session.
func TestSameSessionResumeAssemblesAgain(t *testing.T) {
	var runs string
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		runs = filepath.Join(home, "runs")
		writeFile(t, filepath.Join(dir, "AGENTS.md"), "v1\n```dashy\necho run >> "+runs+"; echo block1\n```\n")
	})
	turn(t, a, "first")
	writeFile(t, filepath.Join(a.Shell.Dir, "AGENTS.md"), "v2\n```dashy\necho run >> "+runs+"; echo block2\n```\n")
	if err := a.Resume(a.SessionID()); err != nil {
		t.Fatal(err)
	}
	turn(t, a, "second")
	turn(t, a, "third")
	data, _ := os.ReadFile(runs)
	if n := strings.Count(string(data), "run"); n != 2 {
		t.Fatalf("blocks ran %d times, want 2", n)
	}
	bodies := fp.requests()
	if !strings.Contains(bodies[0], "block1") || !strings.Contains(bodies[1], "block2") || !strings.Contains(bodies[2], "block2") || strings.Contains(bodies[2], "block1") {
		t.Fatal("request prompts do not follow reassembly and freezing")
	}
}
