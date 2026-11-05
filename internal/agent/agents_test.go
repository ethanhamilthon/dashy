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

func systemPrompt(t *testing.T, global, local string) string {
	t.Helper()
	a := newTestAgentWith(t, &fakeProvider{reply: func(int, string) string { return "" }}, Options{}, func(home, dir string) {
		if global != "" {
			writeFile(t, filepath.Join(home, "DASHY.md"), global)
		}
		if local != "" {
			writeFile(t, filepath.Join(dir, "DASHY.md"), local)
		}
	})
	prompt, err := a.instructions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return prompt
}

func TestPromptKeepsFileBytesVerbatim(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	local := filepath.Join(a.Shell.Dir, "DASHY.md")
	writeFile(t, local, "l\r\nline\n\n\n")
	a.promptFiles = []string{local}
	got, err := a.instructions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := "# " + local + "\nl\r\nline\n\n\n"
	if got != want {
		t.Fatalf("prompt = %q\nwant     %q", got, want)
	}
}

func body(prompt string) string { return prompt[strings.Index(prompt, "\n")+1:] }

func TestOnlyTopLevelDashyBlocksRun(t *testing.T) {
	src := "before\n```dashy\necho one\n```\n- item\n\n  ```dashy\n  echo nested\n  ```\n```sh\necho sh\n```\n> ```dashy\n> echo quoted\n> ```\nafter\n"
	want := "before\none\n- item\n\n  ```dashy\n  echo nested\n  ```\n```sh\necho sh\n```\n> ```dashy\n> echo quoted\n> ```\nafter\n"
	if got := body(systemPrompt(t, src, "")); got != want {
		t.Fatalf("body = %q\nwant   %q", got, want)
	}
}

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

func TestBlockOutputBudget(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	ctx := context.Background()
	exact, err := a.runBlock(ctx, "head -c 63488 /dev/zero | tr '\\0' a", "startup")
	if err != nil || exact != strings.Repeat("a", 62*1024) {
		t.Fatalf("62 KiB of stdout: kept %d bytes, err %v", len(exact), err)
	}
	cut, err := a.runBlock(ctx, "head -c 200000 /dev/zero | tr '\\0' a", "startup")
	if err != nil || len(cut) != blockOutput {
		t.Fatalf("stdout over the limit kept %d bytes, err %v", len(cut), err)
	}
	noisy, err := a.runBlock(ctx, "head -c 100000 /dev/zero | tr '\\0' e >&2; echo out", "startup")
	if err != nil || noisy != "out" {
		t.Fatalf("stderr crowded out stdout: %q, err %v", noisy, err)
	}
	many := "head -c 100000 /dev/zero | tr '\\0' a; for i in $(seq 1 40); do echo line$i >&2; done; exit 2"
	failed, _ := a.runBlock(ctx, many, "startup")
	if !strings.HasSuffix(failed, "line36\nline37\nline38\nline39\nline40") || strings.Count(failed, "\n") != blockTailLines {
		t.Fatalf("stderr tail = %q", failed)
	}
}

func TestSelectedFileVanishingIsAnError(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, func(_, dir string) {
		writeFile(t, filepath.Join(dir, "DASHY.md"), "x")
	})
	if err := os.Remove(filepath.Join(a.Shell.Dir, "DASHY.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.instructions(context.Background()); err == nil {
		t.Fatal("missing selected file was skipped")
	}
}

func TestCancelledTurnStopsWhileBlockRuns(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, func(_, dir string) {
		writeFile(t, filepath.Join(dir, "DASHY.md"), "```dashy\nsleep 30\n```\n")
	})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := a.assemblePrompt(ctx); err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("cancel: %v after %s", err, time.Since(start))
	}
}

func TestSameSessionResumeKeepsSnapshot(t *testing.T) {
	var runs string
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, func(home, dir string) {
		runs = filepath.Join(home, "runs")
		writeFile(t, filepath.Join(dir, "DASHY.md"), "v1\n```dashy\necho run >> "+runs+"; echo block1\n```\n")
	})
	turn(t, a, "first")
	writeFile(t, filepath.Join(a.Shell.Dir, "DASHY.md"), "v2\n```dashy\necho run >> "+runs+"; echo block2\n```\n")
	if err := a.Resume(a.SessionID()); err != nil {
		t.Fatal(err)
	}
	turn(t, a, "second")
	turn(t, a, "third")
	data, _ := os.ReadFile(runs)
	if n := strings.Count(string(data), "run"); n != 1 {
		t.Fatalf("blocks ran %d times, want 1", n)
	}
	bodies := fp.requests()
	if !strings.Contains(bodies[0], "block1") || !strings.Contains(bodies[1], "block1") || !strings.Contains(bodies[2], "block1") || strings.Contains(bodies[2], "block2") {
		t.Fatal("request prompts do not keep the snapshot")
	}
}
