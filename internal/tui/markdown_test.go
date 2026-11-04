package tui

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

func TestMarkdownStylesAndLists(t *testing.T) {
	previous := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	defer lipgloss.SetColorProfile(previous)
	text := "# One\n\n## Two\n\n### Three\n\n#### Four\n\n##### Five\n\n###### Six\n\n`inline blue`\n\n1. First\n2. Second\n   - Nested\n"
	block := renderEntry(entry{Kind: "assistant", Text: text}, 100, -1)
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
		block := renderEntry(entry{Kind: "assistant", Text: text}, 100, -1)
		if !strings.Contains(block, ansi.SetHyperlink("https://example.com/docs")) || !strings.Contains(block, ansi.ResetHyperlink()) {
			t.Fatalf("missing OSC 8 link: %q", block)
		}
	}
	for _, target := range []string{"javascript:alert(1)", "file:///tmp/test", "https://example.com/\x07bad"} {
		if strings.Contains(terminalLink(target, target), "\x1b]8;") {
			t.Errorf("unsafe link allowed: %q", target)
		}
	}
	body := terminalLink("https://example.com/long/path", "https://example.com/long/path")
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
		block := renderEntry(entry{Kind: "assistant", Text: "[Docs](" + target + ")\n\nPlain paragraph."}, width, -1)
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

// Glamour's word wrap does not count hyphens, so a row with one used to overflow and
// break again mid-word: "AGENTS." on one row, "md" alone on the next.
func TestMarkdownWrapWithHyphens(t *testing.T) {
	text := "Загвоздка: мы уже за лимитом, 7131 строка при потолке в 7000. Так что сначала придётся где-то ужать код, иначе AGENTS.md нарушим. Делаем? Ещё раз-два-три и `go test -race`"
	for width := 20; width <= 160; width++ {
		rows := strings.Split(ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text}, width, -1)), "\n")[1:]
		var words []string
		for _, row := range rows {
			if w := ansi.StringWidth(strings.TrimRight(row, " ")); w > width-2 {
				t.Fatalf("width %d: row overflows (%d): %q", width, w, row)
			}
			words = append(words, strings.Fields(row)...)
		}
		if got := strings.Join(words, " "); got != text {
			t.Fatalf("width %d: words were split or changed:\n%q", width, got)
		}
	}
	block := renderEntry(entry{Kind: "assistant", Text: "- one-two\n\n---\n\n| a-b | c |\n|---|---|\n| x | y |\n\nhttps://my-site.dev/a-b"}, 80, -1)
	plain := ansi.Strip(block)
	for _, want := range []string{"• one-two", "a-b", "│ c", "┼"} {
		if !strings.Contains(plain, want) {
			t.Errorf("missing %q in %q", want, plain)
		}
	}
	if strings.Contains(block, "\u2011") || !strings.Contains(block, ansi.SetHyperlink("https://my-site.dev/a-b")) {
		t.Fatalf("hyphens not restored or URL not linked: %q", block)
	}
}

func TestStreamingMarkdownReusesCompletedBlocks(t *testing.T) {
	text := "# Title\n\nFirst paragraph with **bold**.\n\n```go\nx := 1\n\ny := 2\n```\n\nLast one"
	if split := stableSplit(text); text[split:] != "Last one" {
		t.Fatalf("split inside a fence or at the wrong place: %q", text[split:])
	}
	if stableSplit("one paragraph") != 0 || stableSplit("done\n\n") != 0 {
		t.Fatal("text without a following block must render in one piece")
	}
	streamed := ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text, Preview: true}, 80, -1))
	final := ansi.Strip(renderEntry(entry{Kind: "assistant", Text: text}, 80, -1))
	if strings.Join(strings.Fields(streamed), " ") != strings.Join(strings.Fields(final), " ") {
		t.Fatalf("streamed render differs:\n%s\n---\n%s", streamed, final)
	}
	cached := mdStable.out
	renderEntry(entry{Kind: "assistant", Text: text + " grows", Preview: true}, 80, -1)
	if unsafe.StringData(cached) != unsafe.StringData(mdStable.out) {
		t.Fatal("completed blocks were rendered again")
	}
}
