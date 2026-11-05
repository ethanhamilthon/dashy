package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"dashy/internal/agent"
)

const markdownTemplate = `{"document":{"margin":0},"heading":{"bold":true,"block_suffix":"\n"},"h1":{"prefix":"# ","color":"%s"},"h2":{"prefix":"## ","color":"%s"},"h3":{"prefix":"### ","color":"%s"},"h4":{"color":"%s"},"h5":{"color":"%s"},"h6":{"color":"%s"},"strong":{"bold":true},"emph":{"italic":true},"code":{"color":"%s","prefix":"` + "`" + `","suffix":"` + "`" + `"},"code_block":{"margin":0},"list":{"level_indent":2},"item":{"block_prefix":"• "},"enumeration":{"block_prefix":". "},"block_quote":{"indent":1,"indent_token":"│ "},"link":{"underline":true,"color":"%s","format":"\u2063{{.text}}\u2064"},"link_text":{"bold":true},"table":{"center_separator":"┼","column_separator":"│","row_separator":"─"}}`

func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
func formatTokens(n int64) string {
	if n >= 1000000 {
		return fmt.Sprintf("%.1fm", float64(n)/1000000)
	}
	if n >= 1000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprint(n)
}
func formatMetadata(meta *agent.Metadata, kind string) string {
	if kind != "assistant" && kind != "code" && kind != "output" && kind != "error" {
		return ""
	}
	duration := "?"
	if meta != nil {
		duration = formatDuration(meta.Duration)
	}
	if kind == "output" || kind == "error" {
		return "  " + duration
	}
	input, output, cache := "?", "?", "—"
	if meta != nil && meta.Usage != nil {
		u := meta.Usage
		input, output = formatTokens(u.Input), formatTokens(u.Output)
		if u.Input > 0 && u.Cached >= 0 && u.Cached <= u.Input {
			cache = fmt.Sprintf("%.0f%%", 100*float64(u.Cached)/float64(u.Input))
		}
	}
	if kind == "assistant" {
		return fmt.Sprintf("  ↑%s ↓%s · cache %s", input, output, cache)
	}
	return fmt.Sprintf("  ↑%s ↓%s · %s · cache %s", input, output, duration, cache)
}

func formatContext(n int64) string {
	return fmt.Sprintf("%dK", (n+500)/1000)
}

func shortPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		return path
	}
	if path == home {
		return "~"
	}
	if strings.HasPrefix(path, home+string(filepath.Separator)) {
		return "~" + path[len(home):]
	}
	return path
}
