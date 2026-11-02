package tui

import (
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"github.com/charmbracelet/x/ansi"
)

func terminalLink(target string, labels ...string) string {
	text := target
	if len(labels) > 0 {
		text = labels[0]
	}
	u, err := url.Parse(target)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsFunc(target, unicode.IsControl) {
		return text
	}
	return ansi.SetHyperlink(u.String()) + text + ansi.ResetHyperlink()
}

var (
	markedLink        = regexp.MustCompile("(?s)\u2063(.*?)\u2064")
	hyperlinkSequence = regexp.MustCompile("\x1b\\]8;[^\x1b\x07]*(?:\x07|\x1b\\\\)")
)

// Glamour's legacy reflow breaks OSC 8 sequences. Add hyperlinks only after
// rendering, using zero-width markers around URL text from the link formatter.
func markdownLinks(body string) string {
	body = markedLink.ReplaceAllStringFunc(body, func(marked string) string {
		label := strings.TrimSuffix(strings.TrimPrefix(marked, "\u2063"), "\u2064")
		target := strings.Map(func(r rune) rune {
			if unicode.IsSpace(r) {
				return -1
			}
			return r
		}, ansi.Strip(label))
		return terminalLink(target, label)
	})
	return strings.NewReplacer("\u2063", "", "\u2064", "").Replace(body)
}

// Close links on each row so truncation, padding and viewport clipping cannot
// leak a link onto adjacent UI elements. Reopen wrapped links on the next row.
func linkedRows(body string, width int) []string {
	rows := strings.Split(body, "\n")
	active := ""
	for i, row := range rows {
		prefix := active
		for _, seq := range hyperlinkSequence.FindAllString(row, -1) {
			if seq == ansi.ResetHyperlink() {
				active = ""
			} else {
				active = seq
			}
		}
		rows[i] = ansi.Truncate(prefix+row, max(1, width), "…")
		if prefix != "" || strings.Contains(row, "\x1b]8;") {
			rows[i] += ansi.ResetHyperlink()
		}
	}
	return rows
}
