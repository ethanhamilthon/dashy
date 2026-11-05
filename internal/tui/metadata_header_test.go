package tui

import (
	"strings"
	"testing"
	"time"

	"dashy/internal/agent"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestAnswerMetadataInHeader(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(previous) })
	e := entry{Kind: "assistant", Text: "Answer", Meta: &agent.Metadata{At: time.Now(), Duration: 2300 * time.Millisecond}}
	rendered := renderEntryMode(e, 100, -1, false)
	rows := strings.Split(rendered, "\n")
	header := "  * " + answerMetadata(e)
	if len(rows) != 2 || strings.TrimSpace(ansi.Strip(rows[0])) != strings.TrimSpace(header) || !strings.Contains(ansi.Strip(rows[1]), "Answer") {
		t.Fatalf("metadata must follow the star, not the answer: %q", rendered)
	}
	if rows[0] != paint(header, 100, lipgloss.NoColor{}, assistantColor) {
		t.Fatalf("header metadata must use the star color: %q", rows[0])
	}
	e.Preview = true
	if strings.Contains(ansi.Strip(renderEntryMode(e, 100, -1, false)), "cache") {
		t.Fatal("streaming previews must not show final metadata")
	}
}

func TestMinimalHidesAllEvents(t *testing.T) {
	m := bareModel(t, "/project")
	if err := m.ctl.ChangeSettings(func(s *agent.Settings) { s.ShowLines = "minimal" }); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"user", "assistant", "code", "output", "reasoning", "error", "notice", "info", "unknown"} {
		m.entries = append(m.entries, entry{Kind: kind, Text: "event " + kind})
	}
	m.timeline()
	for i, e := range m.entries {
		visible := m.cache[i].Text != ""
		if visible != (e.Kind == "user" || e.Kind == "assistant") {
			t.Errorf("minimal visibility for %s: %t", e.Kind, visible)
		}
	}
}
