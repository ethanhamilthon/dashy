package tui

import (
	"dashy/internal/osc7501"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"

	"dashy/internal/agent"
)

func TestTimelineCacheAndResize(t *testing.T) {
	m := bareModel(t, "/project")
	for i := 0; i < 100; i++ {
		m.add("assistant", fmt.Sprintf("**Answer %d**\n\n- bullet", i))
	}
	m.refresh(true)
	for m.renderPending { // a slow run may spend the render budget before the oldest entry
		m.refresh(true)
	}
	cached := m.cache[0].Text
	m.ctl.Note(agent.Event{Kind: "delta", ID: "stream", Text: "First"})
	m.refresh(true)
	if unsafe.StringData(cached) != unsafe.StringData(m.cache[0].Text) {
		t.Fatal("completed entry was reformatted")
	}
	inputWidth, inputHeight := m.inputWidth, m.inputHeight
	second := agent.Event{Kind: "delta", ID: "stream", Text: " second"}
	m.ctl.Note(second) // the controller applies an agent event before emitting it
	next, _ := m.Update(agentEvent(second))
	m = flushFrame(next.(model)) // the delta came within one frame of the refresh
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
		rendered := ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text}, width, -1))
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
	a := freshAgent(t)
	a.History = []json.RawMessage{json.RawMessage(`{"role":"user","content":"` + strings.Repeat("x", 4400) + `"}`)}
	m := newModel(a, "/a/very/long/项目/path", osc7501.New(io.Discard, false))
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
func TestBlockLineLimits(t *testing.T) {
	text := "first\nsecond\nthird\nfourth\nfifth\nsixth"
	for _, kind := range []string{"code", "output", "reasoning"} {
		for _, limit := range []int{1, 4, 999} {
			e := entry{Kind: kind, Text: text}
			block := ansi.Strip(renderEntry(e, 80, limit))
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
		headerColor, _ := eventColors(kind)
		want := darkGray
		if kind == "code" {
			want = codeGray
		}
		if bodyColor(kind) != want || headerColor != want {
			t.Fatalf("unexpected %s colors", kind)
		}
	}
}

func TestTimelineLimitsAndHiddenSpacing(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	m := newModel(a, "/project", osc7501.New(io.Discard, false))
	text := "first\nsecond\nthird\nfourth\nfifth\nsixth\nseventh\neighth"
	for _, kind := range []string{"code", "output", "reasoning", "assistant"} {
		m.entries = append(m.entries, entry{Kind: kind, Text: text, Meta: &agent.Metadata{}})
	}
	shown := func() (code, output, reasoning, assistant string) {
		m.cache = nil
		m.timeline()
		return ansi.Strip(m.cache[0].Text), ansi.Strip(m.cache[1].Text), ansi.Strip(m.cache[2].Text), ansi.Strip(m.cache[3].Text)
	}
	setLines := func(value string) {
		if err := a.ChangeSettings(func(s *agent.Settings) { s.ShowLines = value }); err != nil {
			t.Fatal(err)
		}
	}
	// minimal: messages only.
	setLines("minimal")
	code, output, reasoning, assistant := shown()
	if code != "" || output != "" || reasoning != "" || !strings.Contains(assistant, "eighth") {
		t.Fatalf("minimal: %q %q %q", code, output, reasoning)
	}
	// normal (the default): 3 lines of the tool call, no output.
	setLines("normal")
	code, output, _, _ = shown()
	if !strings.Contains(code, "third") || strings.Contains(code, "fourth") || output != "" {
		t.Fatalf("normal: code %q output %q", code, output)
	}
	// full: the whole tool call, 6 lines of the output.
	setLines("full")
	code, output, _, _ = shown()
	if !strings.Contains(code, "eighth") || !strings.Contains(output, "sixth") || strings.Contains(output, "seventh") {
		t.Fatalf("full: code %q output %q", code, output)
	}
	for _, e := range m.entries {
		if !strings.Contains(e.Text, "eighth") {
			t.Fatal("UI limit changed source text")
		}
	}
	m.entries = append(m.entries, entry{Kind: "error", Text: "tool failure"})
	if !strings.Contains(ansi.Strip(m.timeline()), "tool failure") {
		t.Fatal("full shows failed tool output")
	}
	for _, mode := range []string{"normal", "minimal"} {
		setLines(mode)
		if strings.Contains(ansi.Strip(m.timeline()), "tool failure") {
			t.Fatalf("%s must hide failed tool output like output", mode)
		}
	}
}

func TestMinimalCommandCompletions(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	m := bareModel(t, "/project")
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
				if !strings.Contains(ansi.Strip(row), cmd.name) || strings.Contains(ansi.Strip(row), "No provider configured") || strings.Contains(ansi.Strip(row), "No model selected") {
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
		for _, row := range rows[len(rows)-2-min(8, len(commands)) : len(rows)-2] {
			if ansi.StringWidth(row) > width {
				t.Fatalf("completion exceeds width %d: %q", width, row)
			}
		}
	}
}

func TestSingleLineNotifications(t *testing.T) {
	note := "<view>Configuration updated. Ready.</view>"
	for _, tc := range []struct{ text, want string }{
		{"<view>Shell session restarted. Previous variables were cleared.</view>", "! · Shell session restarted. Previous variables were cleared."},
		{"<view>Configuration\n  updated.\tReady.</view>", "! · Configuration updated. Ready."},
		{"\x1b[31mProvider event\x1b[0m\r\nreceived", "! · Provider event received"},
		{"", "! · Notice received."},
	} {
		e := entry{Kind: "notice", Text: tc.text, Meta: &agent.Metadata{}}
		for _, width := range []int{1, 2, 5, 20, 80, 120} {
			block := renderEntry(e, width, -1)
			plain := ansi.Strip(block)
			if strings.ContainsAny(plain, "\r\n\t") || ansi.StringWidth(plain) > width {
				t.Fatalf("notice must be one row at width %d: %q", width, plain)
			}
			if width >= 80 && strings.TrimSpace(plain) != tc.want {
				t.Errorf("got %q, want %q", plain, tc.want)
			}
			if strings.Contains(plain, "<view>") {
				t.Fatalf("technical notice text leaked: %q", plain)
			}
		}
		if e.Text != tc.text {
			t.Fatal("notification source text was modified")
		}
	}
	fg, marker := eventColors("notice")
	if fg != muted || marker != "!" {
		t.Fatal("notice must use a gray icon and text")
	}
	t.Setenv("DASHY_HOME", t.TempDir())
	a, err := agent.New(t.TempDir(), agent.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	a.History = []json.RawMessage{json.RawMessage(`{"role":"user","content":"` + note + `"}`)}
	m := newModel(a, "/project", osc7501.New(io.Discard, false))
	restored := m.timeline()
	if !strings.Contains(ansi.Strip(restored), "! · Configuration updated. Ready.") || strings.Contains(ansi.Strip(restored), "<view>") {
		t.Fatal("restored notice not formatted")
	}
	live := bareModel(t, "/project")
	live.ctl.Note(agent.Event{Kind: "notice", Text: note})
	live.refresh(true)
	if m.cache[0].Text != live.cache[0].Text {
		t.Fatal("live and restored notifications differ")
	}
}

func TestStreamedEventsAreCoalescedIntoFrames(t *testing.T) {
	m := bareModel(t, "/project")
	delta := agent.Event{Kind: "delta", ID: "s", Text: "one"}
	m.ctl.Note(delta)
	next, cmd := m.Update(agentEvent(delta)) // right after the initial refresh
	m = next.(model)
	if !m.dirty || !m.redrawScheduled || cmd == nil {
		t.Fatal("a delta within a frame must wait for the scheduled redraw")
	}
	if strings.Contains(ansi.Strip(m.View()), "one") {
		t.Fatal("deferred delta already rendered")
	}
	next, _ = m.Update(redraw{})
	m = next.(model)
	if m.dirty || m.redrawScheduled || !strings.Contains(ansi.Strip(m.View()), "one") {
		t.Fatal("redraw did not apply the delta")
	}
	m.frameAt = time.Now().Add(-time.Second)
	delta = agent.Event{Kind: "delta", ID: "s", Text: " two"}
	m.ctl.Note(delta)
	next, _ = m.Update(agentEvent(delta))
	if m = next.(model); m.dirty || !strings.Contains(ansi.Strip(m.View()), "one two") {
		t.Fatal("a delta after a frame must render at once")
	}
}

func TestRenderBudgetDefersOldMarkdown(t *testing.T) {
	m := bareModel(t, "/project")
	for i := 0; i < 20; i++ {
		m.add("assistant", fmt.Sprintf("**Answer %d**", i))
	}
	m.entries = m.ctl.History()
	m.cache = nil
	m.timelineRows(time.Nanosecond)
	last := m.cache[len(m.cache)-1]
	if !m.renderPending || last.draft || !m.cache[0].draft || !strings.Contains(m.cache[0].Text, "**Answer 0**") {
		t.Fatal("the newest entry must render first and older Markdown wait as plain text")
	}
	for i := 0; m.renderPending; i++ {
		if i > 100 {
			t.Fatal("rendering never completed")
		}
		next, _ := m.Update(redraw{})
		m = next.(model)
	}
	if m.cache[0].draft || strings.Contains(m.cache[0].Text, "**") {
		t.Fatal("deferred Markdown was not rendered")
	}
}
