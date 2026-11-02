package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestMarkdownStylesAndLists(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	text := "# One\n\n## Two\n\n### Three\n\n#### Four\n\n##### Five\n\n###### Six\n\n`inline blue`\n\n1. First\n2. Second\n   - Nested\n"
	block := renderEntry(entry{Kind: "assistant", Text: text}, 100)
	plain := ansi.Strip(block)
	for _, want := range []string{"1. First", "2. Second", "Nested", "inline blue"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in %q", want, plain)
		}
	}
	for _, color := range []string{"155;222;245", "121;217;255", "229;192;123", "135;147;168", "215;195;139", "240;158;172", "138;173;255"} {
		if !strings.Contains(block, "38;2;"+color) {
			t.Errorf("missing RGB %s in %q", color, block)
		}
	}
}

func TestMarkdownHyperlinks(t *testing.T) {
	for _, text := range []string{
		"[Documentation](https://example.com/docs)",
		"<https://example.com/docs>",
		"| Link |\n| --- |\n| [Docs](https://example.com/docs) |",
	} {
		block := renderEntry(entry{Kind: "assistant", Text: text}, 100)
		if !strings.Contains(block, ansi.SetHyperlink("https://example.com/docs")) || !strings.Contains(block, ansi.ResetHyperlink()) {
			t.Fatalf("missing OSC 8 link: %q", block)
		}
	}
	for _, target := range []string{"javascript:alert(1)", "file:///tmp/test", "https://example.com/\x07bad"} {
		if strings.Contains(terminalLink(target), "\x1b]8;") {
			t.Errorf("unsafe link allowed: %q", target)
		}
	}
	body := terminalLink("https://example.com/long/path")
	for _, width := range []int{1, 2, 10, 20} {
		for _, row := range linkedRows(body, width) {
			if ansi.StringWidth(row) > width || !strings.HasSuffix(row, ansi.ResetHyperlink()) {
				t.Fatalf("unbalanced/clipped row: %q", row)
			}
		}
	}
	wrapped := ansi.SetHyperlink("https://example.com") + "first\nsecond" + ansi.ResetHyperlink()
	rows := linkedRows(wrapped, 80)
	if !strings.HasPrefix(rows[1], ansi.SetHyperlink("https://example.com")) {
		t.Fatalf("link not reopened: %q", rows)
	}
}

func TestWrappedMarkdownLinkTargetAndNoLeak(t *testing.T) {
	target := "https://example.com/a/very/long/path?foo=bar&baz=qux"
	for _, width := range []int{1, 2, 5, 10, 20, 80} {
		block := renderEntry(entry{Kind: "assistant", Text: "[Docs](" + target + ")\n\nPlain paragraph."}, width)
		if strings.ContainsAny(block, "\u2063\u2064") {
			t.Fatalf("internal markers leaked at %d", width)
		}
		if width >= 10 && !strings.Contains(block, ansi.SetHyperlink(target)) {
			t.Fatalf("wrapped URL target changed at %d: %q", width, block)
		}
		for _, row := range strings.Split(block, "\n") {
			if ansi.StringWidth(row) > width {
				t.Fatalf("link exceeds width %d: %q", width, row)
			}
			seqs := hyperlinkSequence.FindAllString(row, -1)
			if len(seqs) > 0 && seqs[len(seqs)-1] != ansi.ResetHyperlink() {
				t.Fatalf("row leaks hyperlink: %q", row)
			}
		}
	}
}
