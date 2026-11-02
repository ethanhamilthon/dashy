package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
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

func TestFixedStatusLine(t *testing.T) {
	var n int64 = 37000
	m := model{
		cwd:          "/home/user/project",
		contextInput: &n,
	}
	left, right := m.status()
	if left != "no model selected" {
		t.Fatalf("left = %q, want 'no model selected'", left)
	}
	if !strings.Contains(right, "ctx ~37K") {
		t.Fatalf("right = %q, expected 'ctx ~37K'", right)
	}
}
