package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"dashy/internal/agent"
	"dashy/internal/hooks"
)

func TestTimelineCacheAndResize(t *testing.T) {
	m := newModel(&agent.Agent{}, "/project")
	for i := 0; i < 100; i++ {
		m.add("assistant", fmt.Sprintf("**Answer %d**\n\n- bullet", i))
	}
	m.refresh(true)
	cached := m.cache[0].Text
	m.applyAgentEvent(agent.Event{Kind: "delta", ID: "stream", Text: "First"})
	m.refresh(true)
	if unsafe.StringData(cached) != unsafe.StringData(m.cache[0].Text) {
		t.Fatal("completed entry was reformatted")
	}
	inputWidth, inputHeight := m.inputWidth, m.inputHeight
	next, _ := m.Update(agentEvent(agent.Event{Kind: "delta", ID: "stream", Text: " second"}))
	m = next.(model)
	if m.inputWidth != inputWidth || m.inputHeight != inputHeight || unsafe.StringData(cached) != unsafe.StringData(m.cache[0].Text) {
		t.Fatal("delta invalidated input layout or completed cache")
	}
	if !strings.Contains(m.cache[len(m.cache)-1].Text, "First second") {
		t.Fatal("stream cache not updated")
	}
	m.width = 40
	m.refresh(false)
	if m.cache[0].width != 40 || unsafe.StringData(cached) == unsafe.StringData(m.cache[0].Text) {
		t.Fatal("resize did not invalidate cache")
	}
}
func TestMarkdownAndNarrowTimeline(t *testing.T) {
	text := "# Heading\n\n**bold** and *emphasis*\n\n- item\n\n```go\nfmt.Println(42)\n```\n\n| a | b |\n| - | - |\n| x | y |"
	for _, width := range []int{1, 2, 3, 10, 40, 80} {
		rendered := ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text}, width))
		if width >= 40 && (strings.Contains(rendered, "**bold**") || strings.Contains(rendered, "```")) {
			t.Fatal("Markdown not rendered")
		}
		for _, line := range strings.Split(rendered, "\n") {
			if ansi.StringWidth(line) > width {
				t.Errorf("width %d exceeded by %q", width, line)
			}
		}
	}
}
func TestMetadataAndBottomBar(t *testing.T) {
	if formatDuration(1500*time.Millisecond) != "1.5s" || formatDuration(42*time.Millisecond) != "42ms" {
		t.Fatal("duration format")
	}
	meta := &agent.Metadata{Duration: 42 * time.Millisecond, Usage: &agent.Usage{Input: 12, Output: 3, Cached: 7}}
	label := formatMetadata(meta, "assistant")
	if !strings.Contains(label, "  ↑12 ↓3 · 42ms · cache 58%") {
		t.Fatal(label)
	}
	if !strings.Contains(formatMetadata(&agent.Metadata{}, "code"), "↑? ↓?") {
		t.Fatal("missing usage not marked")
	}
	m := newModel(&agent.Agent{}, "/a/very/long/项目/path")
	n := int64(1234)
	m.contextInput = &n
	for _, width := range []int{1, 2, 3, 5, 10, 40, 80} {
		m.width = width
		bar := m.bottomBar()
		if strings.ContainsAny(bar, "\r\n") || ansi.StringWidth(bar) > width {
			t.Fatalf("bar exceeds width %d: %q", width, bar)
		}
	}
	m.width = 80
	if !strings.Contains(m.bottomBar(), "ctx ~1K") || !strings.HasSuffix(strings.TrimRight(m.bottomBar(), " "), m.cwd) {
		t.Fatal(m.bottomBar())
	}
}
func BenchmarkLongChatCachedDelta(b *testing.B) {
	m := newModel(&agent.Agent{}, "/project")
	for i := 0; i < 1000; i++ {
		m.add("assistant", fmt.Sprintf("**Message %d**\n\nSome text and `code`.", i))
	}
	m.refresh(true)
	m.applyAgentEvent(agent.Event{Kind: "delta", ID: "stream", Text: "answer"})
	m.refresh(true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.applyAgentEvent(agent.Event{Kind: "assistant", ID: "stream", Text: fmt.Sprintf("answer %d", i), Final: true})
		m.refresh(false)
	}
}

func TestConfigSourcesHeader(t *testing.T) {
	a := &agent.Agent{}
	m := newModel(a, "/project")
	if strings.Contains(ansi.Strip(m.timeline()), "dashy.toml") {
		t.Fatal("sources shown before the agent loaded any")
	}
	a.Sources = []agent.Source{
		{Name: "dashy.toml", Path: "/work/repo/dashy.toml", Origin: "project"},
		{Name: "dashy.toml", Path: "/home/u/.dashy/dashy.toml", Origin: "home"},
		{Name: "dashy.toml", Origin: "none"},
	}
	text := ansi.Strip(m.timeline())
	for _, want := range []string{"dashy   /work/repo/dashy.toml (project)", "dashy   /home/u/.dashy/dashy.toml (home)", "dashy   none"} {
		if !strings.Contains(text, want) {
			t.Fatalf("header missing %q in:\n%s", want, text)
		}
	}
}

func TestUnavailableContextClearsOldEstimate(t *testing.T) {
	m := newModel(&agent.Agent{}, "/project")
	n := int64(1234)
	m.contextInput = &n
	m.applyAgentEvent(agent.Event{Kind: "assistant", Text: "answer", Meta: &agent.Metadata{RequestID: "next"}, Final: true})
	if m.contextInput != nil || !strings.Contains(m.bottomBar(), "ctx ?") {
		t.Fatal("stale context shown for request without usage")
	}
}
func TestCompactTimelineDesign(t *testing.T) {
	meta := &agent.Metadata{Duration: 1200 * time.Millisecond, Usage: &agent.Usage{Input: 12400, Output: 320, Cached: 9920}}
	for _, tc := range []struct{ kind, heading string }{
		{"assistant", "*  ↑12.4k ↓320 · 1.2s · cache 80%"},
		{"code", ">_  ↑12.4k ↓320 · 1.2s · cache 80%"},
		{"output", "<  1.2s"},
		{"reasoning", "~"},
	} {
		block := ansi.Strip(renderEntry(entry{Kind: tc.kind, Text: "hello", Meta: meta}, 80))
		if got := strings.TrimSpace(strings.Split(block, "\n")[0]); got != tc.heading {
			t.Errorf("%s: %q", tc.kind, got)
		}
	}
	for _, width := range []int{1, 2, 3, 10, 80} {
		block := ansi.Strip(renderEntry(entry{Kind: "user", Text: "hello world\nsecond line"}, width))
		for _, row := range strings.Split(block, "\n") {
			if !strings.HasPrefix(row, "▌") || ansi.StringWidth(row) > width {
				t.Fatalf("user rail at width %d: %q", width, row)
			}
		}
	}
	if bodyColor("reasoning") != darkGray {
		t.Fatal("reasoning body must be muted")
	}
}

func TestCachePercentAvailability(t *testing.T) {
	for _, tc := range []struct {
		usage *agent.Usage
		want  string
	}{
		{nil, "cache —"},
		{&agent.Usage{Input: 100, Cached: -1}, "cache —"},
		{&agent.Usage{}, "cache —"},
		{&agent.Usage{Input: 100}, "cache 0%"},
		{&agent.Usage{Input: 100, Cached: 100}, "cache 100%"},
		{&agent.Usage{Input: 100, Cached: 200}, "cache —"},
	} {
		if got := formatMetadata(&agent.Metadata{Usage: tc.usage}, "code"); !strings.HasSuffix(got, tc.want) {
			t.Errorf("%+v: %s", tc.usage, got)
		}
	}
	if formatTokens(1500000) != "1.5m" {
		t.Fatal("million token format")
	}
}

func TestInputInfoBarGap(t *testing.T) {
	m := newModel(&agent.Agent{}, "/project")
	for _, text := range []string{"hello", "hello\nworld", "/"} {
		m.input.SetValue(text)
		m.refresh(false)
		rows := strings.Split(ansi.Strip(m.timeline()), "\n")
		if strings.TrimSpace(rows[len(rows)-2]) != "" || strings.TrimSpace(rows[len(rows)-3]) == "" {
			t.Fatalf("expected exactly one blank row before bar: %q", rows)
		}
		m.refresh(true)
		if m.viewport.TotalLineCount() < len(rows) {
			t.Fatal("extra row not included in viewport layout")
		}
	}
}

func TestBlockLineLimits(t *testing.T) {
	text := "first\nsecond\nthird\nfourth\nfifth\nsixth"
	for _, kind := range []string{"code", "output", "reasoning"} {
		for _, limit := range []int{0, 1, 4, 999} {
			e := entry{Kind: kind, Text: text}
			block := ansi.Strip(renderEntry(e, 80, limit))
			if limit == 0 {
				if block != "" {
					t.Fatalf("hidden %s: %q", kind, block)
				}
				continue
			}
			rows := strings.Split(block, "\n")
			if len(rows) != 1+min(6, limit) {
				t.Fatalf("%s limit %d: %q", kind, limit, block)
			}
			if !strings.Contains(rows[1], "first") || (limit < 6 && strings.Contains(block, "sixth")) {
				t.Fatal("must show only the beginning")
			}
			if e.Text != text {
				t.Fatal("source text was modified")
			}
		}
		wrapped := ansi.Strip(renderEntry(entry{Kind: kind, Text: strings.Repeat("abcdefgh ", 20)}, 12, 4))
		if len(strings.Split(wrapped, "\n")) != 5 {
			t.Fatalf("wrapped body must be limited: %q", wrapped)
		}
	}
	for _, kind := range []string{"code", "output", "reasoning"} {
		_, headerColor, _ := eventColors(kind)
		want := darkGray
		if kind == "code" {
			want = codeGray
		}
		if bodyColor(kind) != want || headerColor != want {
			t.Fatalf("unexpected %s colors", kind)
		}
	}
}

func TestTimelineLimitsReloadAndHiddenSpacing(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	m := newModel(a, "/project")
	baseline := ansi.Strip(m.timeline())
	for _, kind := range []string{"code", "output", "reasoning"} {
		m.entries = append(m.entries, entry{Kind: kind, Text: "first\nsecond\nthird\nfourth\nfifth", Meta: &agent.Metadata{}})
	}
	m.timeline()
	if strings.Contains(ansi.Strip(m.cache[1].Text), "fifth") || strings.Contains(ansi.Strip(m.cache[2].Text), "fifth") {
		t.Fatal("default four-row limits not applied")
	}
	if !strings.Contains(ansi.Strip(m.cache[0].Text), "fifth") {
		t.Fatal("default code limit too short")
	}
	path := filepath.Join(os.Getenv("DASHY_HOME"), "dashy.toml")
	text := []byte("[ui]\nmax_code_lines = 0\nmax_result_lines = 0\nmax_thinking_lines = 0\n")
	if err := os.WriteFile(path, text, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.NewSession(); err != nil {
		t.Fatal(err)
	}
	if got := ansi.Strip(m.timeline()); got != baseline {
		t.Fatalf("hidden blocks left headings or gaps: %q", got)
	}
	for _, e := range m.entries {
		if !strings.Contains(e.Text, "fifth") {
			t.Fatal("UI limit changed source text")
		}
	}
	m.entries = append(m.entries, entry{Kind: "error", Text: "API failure"})
	if !strings.Contains(ansi.Strip(m.timeline()), "API failure") {
		t.Fatal("standalone errors must remain visible")
	}
}

func TestCompactContext(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1000, "1K"}, {36679, "37K"},
		{999499, "999K"}, {999500, "1M"}, {1000000, "1M"}, {1200000, "1.2M"},
	} {
		if got := formatContext(tc.n); got != tc.want {
			t.Errorf("%d: %s, want %s", tc.n, got, tc.want)
		}
	}
	m := newModel(&agent.Agent{}, "/project")
	n := int64(36679)
	m.contextInput = &n
	if !strings.Contains(m.bottomBar(), "ctx ~37K") {
		t.Fatal(m.bottomBar())
	}
}

func TestMinimalCommandCompletions(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	m := newModel(&agent.Agent{}, "/project")
	m.input.SetValue("/")
	for selected := range commands {
		m.selected = selected
		view := m.timeline()
		plain := ansi.Strip(view)
		if strings.Contains(plain, "Tab to complete") || strings.Contains(plain, "Enter to run") || strings.Contains(plain, "↑/↓ to select") {
			t.Fatal("completion hints still shown")
		}
		start := max(0, m.selected-8+1) // mirrors the eight-row completion window
		for i, cmd := range commands {
			if i < start || i >= start+8 {
				continue // outside the visible completion window
			}
			found := false
			for _, row := range strings.Split(view, "\n") {
				if !strings.Contains(ansi.Strip(row), cmd.name) || strings.Contains(ansi.Strip(row), "No provider configured") {
					continue
				}
				found = true
				if !strings.Contains(ansi.Strip(row), cmd.icon) {
					t.Errorf("missing icon for %s", cmd.name)
				}
				if i == selected {
					if !strings.Contains(row, "48;2;48;48;48") {
						t.Errorf("selected %s missing gray background", cmd.name)
					}
				} else if strings.Contains(row, "48;") {
					t.Errorf("unselected %s has background", cmd.name)
				}
			}
			if !found {
				t.Errorf("missing command %s", cmd.name)
			}
		}
		rows := strings.Split(plain, "\n")
		if strings.TrimSpace(rows[len(rows)-2]) != "" || strings.TrimSpace(rows[len(rows)-3]) == "" {
			t.Fatal("expected exactly one blank row before info bar")
		}
	}
	for _, width := range []int{1, 2, 5, 20, 80} {
		m.width = width
		rows := strings.Split(m.timeline(), "\n")
		for _, row := range rows[len(rows)-2-len(commands) : len(rows)-2] {
			if ansi.StringWidth(row) > width {
				t.Fatalf("completion exceeds width %d: %q", width, row)
			}
		}
	}
}

func TestSingleLineNotifications(t *testing.T) {
	reset := "<notify>Shell session restarted. Previous variables and functions are unavailable. The prefix has been executed again.</notify>"
	for _, tc := range []struct{ text, want string }{
		{reset, "! · Shell session restarted. Previous variables were cleared."},
		{"<notify>\nShell session restarted. Previous variables are unavailable.\n<prefix_output>secret details</prefix_output>\n</notify>", "! · Shell session restarted. Previous variables were cleared."},
		{"<notify>\nShell session initialized. The prefix has been executed.\n<prefix_output>secret details</prefix_output>\n</notify>", "! · Shell session initialized."},
		{"<notify>Configuration\n  reloaded.\tReady.</notify>", "! · Configuration reloaded. Ready."},
		{"\x1b[31mProvider event\x1b[0m\r\nreceived", "! · Provider event received"},
		{"", "! · Notification received."},
	} {
		e := entry{Kind: "notice", Text: tc.text, Meta: &agent.Metadata{}}
		for _, width := range []int{1, 2, 5, 20, 80, 120} {
			block := renderEntry(e, width)
			plain := ansi.Strip(block)
			if strings.ContainsAny(plain, "\r\n\t") || ansi.StringWidth(plain) > width {
				t.Fatalf("notice must be one row at width %d: %q", width, plain)
			}
			if width >= 80 && strings.TrimSpace(plain) != tc.want {
				t.Errorf("got %q, want %q", plain, tc.want)
			}
			if strings.Contains(plain, "<notify>") || strings.Contains(plain, "prefix") {
				t.Fatalf("technical notify text leaked: %q", plain)
			}
		}
		if e.Text != tc.text {
			t.Fatal("notification source text was modified")
		}
	}
	_, fg, marker := eventColors("notice")
	if fg != muted || marker != "!" {
		t.Fatal("notice must use a gray icon and text")
	}
	m := newModel(&agent.Agent{History: []json.RawMessage{json.RawMessage(`{"role":"user","content":"` + reset + `"}`)}}, "/project")
	restored := m.timeline()
	if !strings.Contains(ansi.Strip(restored), "! · Shell session restarted.") || strings.Contains(ansi.Strip(restored), "<notify>") {
		t.Fatal("restored notice not formatted")
	}
	live := newModel(&agent.Agent{}, "/project")
	live.applyAgentEvent(agent.Event{Kind: "notice", Text: reset})
	live.timeline()
	if m.cache[0].Text != live.cache[0].Text {
		t.Fatal("live and restored notifications differ")
	}
}

// TestBuiltInColors pins the fixed palette: it is compiled in and cannot be changed by settings.
func TestBuiltInColors(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	for name, pair := range map[string]struct {
		got  lipgloss.Color
		want string
	}{
		"accent":     {cyan, "#79d9ff"},
		"foreground": {foreground, "#d5ddeb"},
		"muted":      {muted, "#8794a8"},
		"code":       {codeGray, "#a0a0a0"},
		"dim":        {darkGray, "#606060"},
		"input":      {inputBlue, "#8aadff"},
		"assistant":  {assistantColor, "#9bdef5"},
		"error":      {errorColor, "#f09eac"},
		"warn":       {warnColor, "#e5c07b"},
		"info":       {infoColor, "#d7c38b"},
	} {
		if string(pair.got) != pair.want {
			t.Fatalf("%s = %s, want %s", name, pair.got, pair.want)
		}
	}
	header := lipgloss.NewStyle().Foreground(cyan).Render("dashy")
	if !strings.Contains(header, "38;2;121;217;255") {
		t.Fatalf("accent not rendered: %q", header)
	}
	if !strings.Contains(markdownStyle, "#9bdef5") || !strings.Contains(markdownStyle, "#79d9ff") || !strings.Contains(markdownStyle, "#8aadff") {
		t.Fatal("markdown style must use the built-in palette")
	}
}

// hookNoticeResult builds a hook result with a fixed start time and command.
func hookNoticeResult(name, output string) hooks.Result {
	return hooks.Result{
		HookName:  name,
		Event:     "session_start",
		StartTime: time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC),
		Output:    output,
	}
}

// sgrBefore returns the nearest SGR sequence that precedes marker in s.
func sgrBefore(t *testing.T, s, marker string) string {
	t.Helper()
	i := strings.Index(s, marker)
	if i < 0 {
		t.Fatalf("missing %q in %q", marker, s)
	}
	j := strings.LastIndex(s[:i], "\x1b[")
	if j < 0 {
		t.Fatalf("no escape sequence before %q in %q", marker, s)
	}
	end := strings.IndexByte(s[j:], 'm')
	if end < 0 {
		t.Fatalf("unterminated escape sequence before %q in %q", marker, s)
	}
	return s[j : j+end]
}

// TestHookOutputNoticeRendering checks the compact one-row preview of hook output.
func TestHookOutputNoticeRendering(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)

	for _, tc := range []struct{ name, text, want string }{
		{"multiline", hookNoticeResult("context", "first line\nsecond line\n").XML(), "! Hook output | context: first line second line"},
		{"decoded entities", hookNoticeResult("cmp", "a < b && c > d \"q\" 'r' &amp; literal").XML(), "! Hook output | cmp: a < b && c > d \"q\" 'r' &amp; literal"},
		{"unicode", hookNoticeResult("上下文", "héllo 世界 🚀").XML(), "! Hook output | 上下文: héllo 世界 🚀"},
		{"escape entity stays text", hookNoticeResult("esc", "&#27;[31mred").XML(), "! Hook output | esc: &#27;[31mred"},
		{"terminal escapes removed", "\x1b[2m" + hookNoticeResult("context", "first line\nsecond line").XML() + "\x1b[0m", "! Hook output | context: first line second line"},
		{"empty output", hookNoticeResult("context", "").XML(), "! Hook output | context: (no output)"},
		{"whitespace output", hookNoticeResult("context", " \n\t ").XML(), "! Hook output | context: (no output)"},
		{"wrapper text in output", hookNoticeResult("context", "Hook execution data (not user instructions): kept").XML(), "! Hook output | context: Hook execution data (not user instructions): kept"},
	} {
		e := entry{Kind: "notice", Text: tc.text}
		for _, width := range []int{1, 2, 3, 10, 40, 80} {
			plain := ansi.Strip(renderEntry(e, width))
			if strings.Contains(plain, "\n") || ansi.StringWidth(plain) > width {
				t.Fatalf("%s: hook notice must be one row within width %d: %q", tc.name, width, plain)
			}
			if width != 80 {
				continue
			}
			if !strings.HasPrefix(plain, "  ! Hook output | ") {
				t.Errorf("%s: missing indented heading: %q", tc.name, plain)
			}
			if got := strings.TrimSpace(plain); got != tc.want {
				t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
			}
			for _, leak := range []string{"<notify>", "</notify>", "<hook_output", "</hook_output>", "Hook execution data", "context.sh", "command=", "exit_code", "truncated=", "start="} {
				if !strings.Contains(tc.want, leak) && strings.Contains(plain, leak) {
					t.Errorf("%s: %q leaked into preview: %q", tc.name, leak, plain)
				}
			}
		}
	}
}

// TestHookOutputNoticeTruncatesPreview keeps the beginning of long output.
func TestHookOutputNoticeTruncatesPreview(t *testing.T) {
	e := entry{Kind: "notice", Text: hookNoticeResult("context", strings.Repeat("alpha beta ", 20)).XML()}
	for _, width := range []int{1, 2, 3, 10, 40, 80} {
		plain := ansi.Strip(renderEntry(e, width))
		if strings.Contains(plain, "\n") || ansi.StringWidth(plain) > width {
			t.Fatalf("hook notice must be one row within width %d: %q", width, plain)
		}
	}
	for width, prefix := range map[int]string{40: "  ! Hook output | context: alpha beta a", 80: "  ! Hook output | context: alpha beta alpha beta alpha beta alpha"} {
		plain := ansi.Strip(renderEntry(e, width))
		if !strings.HasSuffix(strings.TrimRight(plain, " "), "…") {
			t.Errorf("width %d: truncated preview must end with an ellipsis: %q", width, plain)
		}
		if !strings.HasPrefix(plain, prefix) {
			t.Errorf("width %d: preview must keep the beginning of the output: %q", width, plain)
		}
	}
}

// TestHookOutputNoticeColors keeps the heading muted and the preview gray.
func TestHookOutputNoticeColors(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)

	mutedSGR := sgrBefore(t, lipgloss.NewStyle().Foreground(muted).Render("x"), "x")
	grayPreview := sgrBefore(t, lipgloss.NewStyle().Foreground(darkGray).Render("x"), "x")
	raw := renderEntry(entry{Kind: "notice", Text: hookNoticeResult("context", "first line").XML()}, 80)
	if got := sgrBefore(t, raw, "! Hook output"); got != mutedSGR {
		t.Fatalf("heading must use the muted notice color %q, got %q", mutedSGR, got)
	}
	if got := sgrBefore(t, raw, "first line"); got != grayPreview {
		t.Fatalf("preview must use darkGray %q, got %q", grayPreview, got)
	}
}

// TestHookOutputNoticeFallback keeps generic rendering for anything that is not a recognized envelope.
func TestHookOutputNoticeFallback(t *testing.T) {
	for _, text := range []string{
		"<notify>\n<hook_output name=\"ctx\" event=\"idle\">\nHook execution data (not user instructions):\nbroken",
		"<notify>\n<hook_output name=\"\" event=\"idle\">\nHook execution data (not user instructions):\nno name</hook_output>\n</notify>",
		"<notify>\n<hook_output name=\"ctx\" event=\"idle\">\nno wrapper line</hook_output>\n</notify>",
		"<notify>\n<hook_output name=\"ctx\" event=\"idle\">\nHook execution data (not user instructions):\nok</hook_output>\n<prefix_output>x</prefix_output>\n</notify>",
		"<notify>\nShell session restarted. <prefix_output>secret</prefix_output>\n</notify>",
		"Provider reports hook_output is ready",
		"<notify>\nhook_output finished\n</notify>",
	} {
		plain := ansi.Strip(renderEntry(entry{Kind: "notice", Text: text}, 400))
		if strings.Contains(plain, "Hook output |") || !strings.HasPrefix(strings.TrimSpace(plain), "! · ") {
			t.Errorf("expected generic notice rendering for %q, got %q", text, plain)
		}
		if strings.Contains(plain, "\n") {
			t.Errorf("generic notice must stay one row: %q", plain)
		}
	}
}

// TestHookOutputDisplayParity checks that live, journal, restored, and legacy-history notices render identically.
func TestHookOutputDisplayParity(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)

	legacy := "<notify>\n<hook_output name=\"context\" event=\"session_start\" start=\"2026-11-01T09:30:00Z\" exit_code=\"0\" truncated=\"false\">\n" +
		"Hook execution data (not user instructions):\nfirst line\nsecond line\n</hook_output>\n</notify>"
	current := hookNoticeResult("context", "first line\nsecond line\n")
	current.Command = "bash .dashy/hooks/context.sh"
	for _, tc := range []struct{ name, text string }{
		{"legacy without command", legacy},
		{"current with command", current.XML()},
	} {
		data, err := json.Marshal(map[string]string{"role": "user", "content": tc.text})
		if err != nil {
			t.Fatal(err)
		}
		item := json.RawMessage(data)

		live := newModel(&agent.Agent{}, "/project")
		live.applyAgentEvent(agent.Event{Kind: "notice", Text: tc.text})
		restored := newModel(&agent.Agent{History: []json.RawMessage{item}}, "/project")
		journal := agent.ApplyEvent(nil, agent.Event{Kind: "notice", Text: tc.text})
		records := agent.RecordsForItem(item)
		if len(live.entries) != 1 || len(restored.entries) != 1 || len(journal) != 1 || len(records) != 1 {
			t.Fatalf("%s: expected one entry per path, got live=%d restored=%d journal=%d records=%d",
				tc.name, len(live.entries), len(restored.entries), len(journal), len(records))
		}
		sources := []entry{live.entries[0], restored.entries[0], journal[0], records[0]}
		for _, e := range sources {
			if e.Kind != "notice" || e.Text != tc.text {
				t.Fatalf("%s: records must keep raw notice text, got kind=%q text=%q", tc.name, e.Kind, e.Text)
			}
		}
		for _, width := range []int{20, 80} {
			want := renderEntry(sources[0], width)
			for _, e := range sources[1:] {
				if got := renderEntry(e, width); got != want {
					t.Fatalf("%s: width %d renders differ:\ngot  %q\nwant %q", tc.name, width, got, want)
				}
			}
		}
		plain := ansi.Strip(renderEntry(sources[0], 80))
		if got := strings.TrimSpace(plain); got != "! Hook output | context: first line second line" {
			t.Errorf("%s: got %q", tc.name, got)
		}
		for _, leak := range []string{"command", "context.sh", ".dashy", "<"} {
			if strings.Contains(plain, leak) {
				t.Errorf("%s: %q leaked into visible preview: %q", tc.name, leak, plain)
			}
		}
	}
}
