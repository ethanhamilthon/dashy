package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/pelletier/go-toml/v2"

	"dashy/internal/agent"
	"dashy/internal/defaults"
)

func TestBottomBarWidthsAndPadding(t *testing.T) {
	left := "openai · gpt-4o · ctx ~1K"
	right := "~/projects/dashy"

	for _, width := range []int{0, 1, 2, 3, 4, 5, 6, 10, 20, 40, 80, 120} {
		bar := renderBottomBar(width, left, right)
		if strings.ContainsAny(bar, "\r\n") {
			t.Fatalf("width %d: bar contains newline: %q", width, bar)
		}
		sw := ansi.StringWidth(bar)
		if sw > width {
			t.Fatalf("width %d: bar exceeds width (sw=%d): %q", width, sw, bar)
		}
		if width >= 5 {
			if !strings.HasPrefix(bar, "  ") {
				t.Fatalf("width %d: missing 2 leading spaces: %q", width, bar)
			}
			if !strings.HasSuffix(bar, "  ") {
				t.Fatalf("width %d: missing 2 trailing spaces: %q", width, bar)
			}
			avail := width - 4
			content := bar[2 : len(bar)-2]
			if ansi.StringWidth(content) > avail {
				t.Fatalf("width %d: content exceeds available width: %q", width, content)
			}
		}
	}
}

func TestBottomBarUnicodeTruncation(t *testing.T) {
	// Unicode wide characters and emojis
	left := "测试项目 · 模型/high · 很高兴"
	right := "🚀/测试/路径/很长很长的路径名称"
	for _, width := range []int{1, 3, 4, 5, 10, 25, 40, 80} {
		bar := renderBottomBar(width, left, right)
		if strings.ContainsAny(bar, "\r\n") {
			t.Fatalf("width %d contains newline", width)
		}
		sw := ansi.StringWidth(bar)
		if sw > width {
			t.Fatalf("width %d: Unicode bar width %d exceeds max: %q", width, sw, bar)
		}
		if width >= 5 {
			if !strings.HasPrefix(bar, "  ") || !strings.HasSuffix(bar, "  ") {
				t.Fatalf("width %d: missing 2 spaces on edges: %q", width, bar)
			}
		}
	}
}

func TestStatusCommandsExecutionAndJoining(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)
	snap := StatusSnapshot{
		Cwd: t.TempDir(),
		LeftCmds: []string{
			`echo "first"`,
			`echo ""`, // empty: omitted
			`exit 1`,  // failed: omitted
			`echo "second part"`,
		},
		RightCmds: []string{
			`echo "right1"`,
			`echo "right2"`,
		},
	}

	left, right := evalStatusSnapshot(ctx, snap, sem)
	if left != "first · second part" {
		t.Fatalf("unexpected left segments: %q", left)
	}
	if right != "right1 · right2" {
		t.Fatalf("unexpected right segments: %q", right)
	}
}

func TestStatusEnvironmentAndMetadata(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	snap := StatusSnapshot{
		Cwd: dir,
		Environment: map[string]string{
			"CUSTOM_CONFIG": "custom_val",
			"BASH_ENV":      "/should/not/be/set",
		},
		Metadata: map[string]string{
			"DASHY_PROVIDER":   "anthropic",
			"DASHY_MODEL":      "claude-3-7-sonnet",
			"DASHY_EFFORT":     "medium",
			"DASHY_CTX_SIZE":   "42000",
			"DASHY_CWD":        dir,
			"DASHY_SESSION_ID": "sess-test-456",
			"DASHY_VERSION":    "0.6.0",
			"DASHY_STATE":      "busy",
			"DASHY_EVENT":      "model_request_start",
		},
		LeftCmds: []string{
			`echo "$DASHY_PROVIDER:$DASHY_MODEL/$DASHY_EFFORT:$DASHY_STATE:$DASHY_EVENT"`,
			`echo "$CUSTOM_CONFIG"`,
			`test -z "$BASH_ENV" && echo "bash_env_clean"`,
			`[ "$PWD" = "$DASHY_CWD" ] && echo "cwd_clean"`,
		},
		RightCmds: []string{
			`echo "ctx:$DASHY_CTX_SIZE:v$DASHY_VERSION:id=$DASHY_SESSION_ID"`,
		},
	}

	left, right := evalStatusSnapshot(ctx, snap, sem)
	expectedLeft := "anthropic:claude-3-7-sonnet/medium:busy:model_request_start · custom_val · bash_env_clean · cwd_clean"
	if left != expectedLeft {
		t.Fatalf("left = %q, want %q", left, expectedLeft)
	}
	expectedRight := "ctx:42000:v0.6.0:id=sess-test-456"
	if right != expectedRight {
		t.Fatalf("right = %q, want %q", right, expectedRight)
	}
}

func TestStatusStdoutCap1KiB(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)
	snap := StatusSnapshot{
		Cwd: t.TempDir(),
		LeftCmds: []string{
			// Generate 5000 characters
			`head -c 5000 /dev/zero | tr '\0' 'A'`,
		},
	}

	left, _ := evalStatusSnapshot(ctx, snap, sem)
	if len(left) > 1024 {
		t.Fatalf("expected stdout cap <= 1024 bytes, got %d", len(left))
	}
	if len(left) != 1024 {
		t.Fatalf("expected exactly 1024 bytes capped, got %d", len(left))
	}
}

func TestStatusDeadline500ms(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)
	start := time.Now()
	snap := StatusSnapshot{
		Cwd: t.TempDir(),
		LeftCmds: []string{
			`sleep 2; echo "should timeout"`,
		},
		RightCmds: []string{
			`echo "fast"`,
		},
	}

	left, right := evalStatusSnapshot(ctx, snap, sem)
	elapsed := time.Since(start)
	if left != "" {
		t.Fatalf("timed out command should be omitted, got %q", left)
	}
	if right != "fast" {
		t.Fatalf("fast command should succeed, got %q", right)
	}
	if elapsed > 1500*time.Millisecond {
		t.Fatalf("execution took too long (%v), timeout should kill process group around 500ms", elapsed)
	}
}

func TestStatusSanitization(t *testing.T) {
	dirty := "\x1b[31;1mRed Bold\x1b[0m\r\n\tSecond Line\x00\x07\nThird"
	cleaned := sanitizeStatusSegment(dirty)
	if strings.ContainsAny(cleaned, "\r\n\t\x00\x07") {
		t.Fatalf("dirty control characters not sanitized: %q", cleaned)
	}
	if strings.Contains(cleaned, "\x1b") {
		t.Fatalf("ANSI escapes not stripped: %q", cleaned)
	}
	if cleaned != "Red Bold  Second Line Third" {
		t.Fatalf("unexpected cleaned output: %q", cleaned)
	}
}

func TestStatusGenerationAndStaleRejection(t *testing.T) {
	m := newModel(&agent.Agent{}, t.TempDir())
	m.statusGen = 5
	m.statusLeft = "gen5-left"
	m.statusRight = "gen5-right"

	// Old generation result arrives
	next, _ := m.Update(statusResultMsg{gen: 4, left: "stale-left", right: "stale-right"})
	nm := next.(model)
	if nm.statusLeft != "gen5-left" || nm.statusRight != "gen5-right" {
		t.Fatalf("stale generation 4 overwrote current generation: %q, %q", nm.statusLeft, nm.statusRight)
	}

	// Current generation result arrives
	next, _ = m.Update(statusResultMsg{gen: 5, left: "fresh-left", right: "fresh-right"})
	nm = next.(model)
	if nm.statusLeft != "fresh-left" || nm.statusRight != "fresh-right" {
		t.Fatalf("fresh generation 5 failed to update: %q, %q", nm.statusLeft, nm.statusRight)
	}
}

func TestStatusEmptyHidesSide(t *testing.T) {
	m := newModel(&agent.Agent{}, t.TempDir())
	m.statusInit = true
	m.statusLeft = ""
	m.statusRight = ""

	bar := m.bottomBar()
	// Both sides empty: when width=80, should be 80 spaces
	if ansi.StringWidth(bar) != 80 {
		t.Fatalf("expected width 80 for empty bar, got %d", ansi.StringWidth(bar))
	}
	if strings.TrimSpace(bar) != "" {
		t.Fatalf("expected all spaces when status is empty, got %q", bar)
	}
}

func TestStatusConcurrencyBounded(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)
	snap := StatusSnapshot{
		Cwd: t.TempDir(),
		LeftCmds: []string{
			`sleep 0.05; echo "1"`,
			`sleep 0.05; echo "2"`,
			`sleep 0.05; echo "3"`,
			`sleep 0.05; echo "4"`,
			`sleep 0.05; echo "5"`,
		},
	}

	left, _ := evalStatusSnapshot(ctx, snap, sem)
	if left != "1 · 2 · 3 · 4 · 5" {
		t.Fatalf("expected all 5 ordered segments, got %q", left)
	}
}

func TestDefaultBashFormatting(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)
	home, _ := os.UserHomeDir()
	demoCwd := filepath.Join(home, "work", "demo")

	// The shipped defaults are the single source of truth for the status bar.
	var shipped struct {
		UI struct {
			Left  []string `toml:"status_left"`
			Right []string `toml:"status_right"`
		} `toml:"ui"`
	}
	if err := toml.Unmarshal([]byte(defaults.Config), &shipped); err != nil {
		t.Fatal(err)
	}

	snap := StatusSnapshot{
		Cwd: demoCwd,
		Metadata: map[string]string{
			"DASHY_PROVIDER": "anthropic",
			"DASHY_MODEL":    "claude",
			"DASHY_EFFORT":   "",
			"DASHY_CTX_SIZE": "36679",
			"DASHY_CWD":      demoCwd,
		},
		LeftCmds:  shipped.UI.Left,
		RightCmds: shipped.UI.Right,
	}

	left, right := evalStatusSnapshot(ctx, snap, sem)
	if left != "anthropic · claude · ctx ~37K" {
		t.Fatalf("default left = %q, want 'anthropic · claude · ctx ~37K'", left)
	}
	if right != "~/work/demo" {
		t.Fatalf("default right = %q, want '~/work/demo'", right)
	}
}

func TestStatusPayloadRegressionExecStillWorks(t *testing.T) {
	ctx := context.Background()
	sem := make(chan struct{}, 4)

	hugeMessage := strings.Repeat("M", 200*1024)  // 200 KB
	hugeCommand := strings.Repeat("C", 1024*1024) // 1 MB
	hugeNotify := strings.Repeat("N", 1024*1024)  // 1 MB

	// Test 1: Via triggerStatusRefresh with model
	m := newModel(&agent.Agent{}, t.TempDir())
	eventVars := map[string]string{
		"DASHY_EVENT":        "turn_start",
		"DASHY_TURN_SOURCE":  "user",
		"DASHY_MESSAGE_FILE": hugeMessage,
		"DASHY_COMMAND_FILE": hugeCommand,
		"DASHY_NOTIFY_FILE":  hugeNotify,
		"RANDOM_UNALLOWED":   "drop_me",
	}
	cmd := m.triggerStatusRefresh(eventVars)
	if cmd == nil {
		t.Fatal("expected non-nil cmd from triggerStatusRefresh")
	}

	// Test 2: StatusSnapshot evaluation with commands checking for payload leak
	snap := StatusSnapshot{
		Cwd: t.TempDir(),
		Metadata: map[string]string{
			"DASHY_EVENT":       "turn_start",
			"DASHY_TURN_SOURCE": "user",
			// Maliciously / accidentally injected payloads:
			"DASHY_MESSAGE_FILE": hugeMessage,
			"DASHY_COMMAND_FILE": hugeCommand,
			"DASHY_NOTIFY_FILE":  hugeNotify,
			"RANDOM_UNALLOWED":   "drop_me",
		},
		LeftCmds: []string{
			`echo "event=$DASHY_EVENT source=$DASHY_TURN_SOURCE"`,
			`echo "msg=${DASHY_MESSAGE_FILE:-none} cmd=${DASHY_COMMAND_FILE:-none} notify=${DASHY_NOTIFY_FILE:-none} unallowed=${RANDOM_UNALLOWED:-none}"`,
		},
		RightCmds: []string{
			// Verify that env size is small and execution succeeds without E2BIG (argument list too long)
			`echo "exec_ok"`,
		},
	}

	left, right := evalStatusSnapshot(ctx, snap, sem)
	expectedLeft := "event=turn_start source=user · msg=none cmd=none notify=none unallowed=none"
	if left != expectedLeft {
		t.Fatalf("left = %q, want %q", left, expectedLeft)
	}
	if right != "exec_ok" {
		t.Fatalf("right = %q, want 'exec_ok'", right)
	}
}

func TestStatusCancellationNotLogged(t *testing.T) {
	home := t.TempDir()
	logFile := filepath.Join(home, "status.log")

	statusLogMu.Lock()
	lastStatusLog = time.Time{}
	statusLogMu.Unlock()

	sem := make(chan struct{}, 4)
	snap := StatusSnapshot{
		Home: home,
		Cwd:  t.TempDir(),
		LeftCmds: []string{
			`sleep 1`,
		},
	}

	// 1. Cancelled execution: should not log anything to status.log
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = evalStatusSnapshot(ctx, snap, sem)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	<-done

	if _, err := os.Stat(logFile); err == nil {
		content, _ := os.ReadFile(logFile)
		t.Fatalf("status.log should not exist after normal cancellation, found: %q", string(content))
	}

	// 2. Genuine failure: should log immediately (not blocked by rate-limit since cancellation didn't log)
	snapFail := StatusSnapshot{
		Home: home,
		Cwd:  t.TempDir(),
		LeftCmds: []string{
			`exit 42`,
		},
	}
	_, _ = evalStatusSnapshot(context.Background(), snapFail, sem)

	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatalf("expected status.log to exist for genuine failure: %v", err)
	}
	if !strings.Contains(string(content), "cmd=\"exit 42\"") {
		t.Fatalf("expected genuine failure logged, got: %q", string(content))
	}

	// 3. Deadline exceeded: should also log
	home2 := t.TempDir()
	logFile2 := filepath.Join(home2, "status.log")
	statusLogMu.Lock()
	lastStatusLog = time.Time{}
	statusLogMu.Unlock()

	snapTimeout := StatusSnapshot{
		Home: home2,
		Cwd:  t.TempDir(),
		LeftCmds: []string{
			`sleep 2`,
		},
	}
	_, _ = evalStatusSnapshot(context.Background(), snapTimeout, sem)

	content2, err2 := os.ReadFile(logFile2)
	if err2 != nil {
		t.Fatalf("expected status.log for deadline exceeded: %v", err2)
	}
	if !strings.Contains(string(content2), "cmd=\"sleep 2\"") {
		t.Fatalf("expected deadline failure logged, got: %q", string(content2))
	}
}

func TestStatusEscapedDescendantCannotHangSlot(t *testing.T) {
	ctx := context.Background()
	// Strict single slot to ensure slot is not pinned
	sem := make(chan struct{}, 1)
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "descendant.pid")

	t.Cleanup(func() {
		// Clean up specifically the test descendant process to avoid leaving orphan sleep
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	snap := StatusSnapshot{
		Cwd:  dir,
		Home: dir,
		LeftCmds: []string{
			// Spawn background descendant that inherits stdout (holding pipe open)
			fmt.Sprintf("sleep 5 & echo $! > %q; echo parent_done", pidFile),
		},
	}

	start := time.Now()
	_, _ = evalStatusSnapshot(ctx, snap, sem)
	elapsed := time.Since(start)

	// With WaitDelay ~100ms, this command should return in ~100-250ms, well below 5 seconds.
	if elapsed > 1*time.Second {
		t.Fatalf("command took %v; expected WaitDelay ~100ms to unblock stdout pipe", elapsed)
	}

	// Verify the semaphore slot is freed and can immediately run another command
	snapNext := StatusSnapshot{
		Cwd: dir,
		LeftCmds: []string{
			`echo "slot_free"`,
		},
	}
	left, _ := evalStatusSnapshot(ctx, snapNext, sem)
	if left != "slot_free" {
		t.Fatalf("expected slot to be free, got: %q", left)
	}
}
