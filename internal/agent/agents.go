package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"dashy/internal/shell"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"
)

const (
	DashyBlockTimeout   = 30 * time.Second
	DashyBlockMaxOutput = 64 * 1024 // 64 KiB
)

var dashyBlockTimeout = DashyBlockTimeout

type dashyBlock struct {
	start  int
	end    int
	script string
}

func parseDashyBlocks(source []byte) []dashyBlock {
	doc := goldmark.DefaultParser().Parse(text.NewReader(source))
	var blocks []dashyBlock
	_ = ast.Walk(doc, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		cb, ok := n.(*ast.FencedCodeBlock)
		if !ok {
			return ast.WalkContinue, nil
		}
		// Blocks nested inside other blocks/quotes-of-code are NOT run
		if cb.Parent() == nil || cb.Parent().Kind() != ast.KindDocument {
			return ast.WalkContinue, nil
		}
		if cb.Info == nil {
			return ast.WalkContinue, nil
		}
		infoText := string(cb.Info.Text(source))
		if infoText != "dashy" {
			return ast.WalkContinue, nil
		}
		infoSeg := cb.Info.Segment
		start := infoSeg.Start
		for start > 0 && source[start-1] != '\n' {
			start--
		}
		idx := start
		for idx < infoSeg.Start && (source[idx] == ' ' || source[idx] == '\t') {
			idx++
		}
		if idx >= infoSeg.Start {
			return ast.WalkContinue, nil
		}
		fenceChar := source[idx]
		fenceLen := 0
		for idx < infoSeg.Start && source[idx] == fenceChar {
			fenceLen++
			idx++
		}
		script := string(cb.Lines().Value(source))
		var afterBody int
		if cb.Lines().Len() > 0 {
			lastSeg := cb.Lines().At(cb.Lines().Len() - 1)
			afterBody = lastSeg.Stop
		} else {
			afterBody = infoSeg.Stop
			for afterBody < len(source) && source[afterBody] != '\n' {
				afterBody++
			}
			if afterBody < len(source) && source[afterBody] == '\n' {
				afterBody++
			}
		}
		end := len(source)
		if afterBody < len(source) {
			pos := afterBody
			for pos < len(source) {
				lineStart := pos
				for pos < len(source) && source[pos] != '\n' {
					pos++
				}
				lineEnd := pos
				if lineEnd > lineStart && source[lineEnd-1] == '\r' {
					lineEnd--
				}
				if pos < len(source) && source[pos] == '\n' {
					pos++
				}
				p := lineStart
				spaces := 0
				for p < lineEnd && source[p] == ' ' && spaces < 3 {
					spaces++
					p++
				}
				cCount := 0
				for p < lineEnd && source[p] == fenceChar {
					cCount++
					p++
				}
				if cCount >= fenceLen {
					allSpace := true
					for p < lineEnd {
						if source[p] != ' ' && source[p] != '\t' {
							allSpace = false
							break
						}
						p++
					}
					if allSpace {
						end = lineEnd
						break
					}
				}
			}
		}
		blocks = append(blocks, dashyBlock{start: start, end: end, script: script})
		return ast.WalkContinue, nil
	})
	return blocks
}

func substituteBlocks(source []byte, blocks []dashyBlock, repls []string) string {
	var buf strings.Builder
	last := 0
	for i, b := range blocks {
		buf.Write(source[last:b.start])
		buf.WriteString(repls[i])
		last = b.end
	}
	buf.Write(source[last:])
	return buf.String()
}

type limitWriter struct {
	limit int
	buf   bytes.Buffer
}

func newLimitWriter(limit int) *limitWriter {
	return &limitWriter{limit: limit}
}

func (w *limitWriter) Write(p []byte) (int, error) {
	n := len(p)
	remaining := w.limit - w.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		w.buf.Write(p)
	}
	return n, nil
}

func (w *limitWriter) String() string {
	return w.buf.String()
}

func shortTail(s string, maxLines int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

func formatDashyBlockFailure(reason, stderr string) string {
	res := fmt.Sprintf("[dashy block failed: %s]", reason)
	if tail := shortTail(stderr, 5); tail != "" {
		res += "\n" + tail
	}
	return res
}

func (a *Agent) dashyBlockEnv(mode, source string) []string {
	envMap := make(map[string]string)
	for _, entry := range os.Environ() {
		if k, v, ok := strings.Cut(entry, "="); ok {
			if k == "BASH_ENV" || k == "DASHY_ENDPOINT" || k == "DASHY_MODE" || k == "DASHY_SOURCE" || k == "DASHY_SESSION_ID" || k == "DASHY_HOME" {
				continue
			}
			envMap[k] = v
		}
	}
	envMap["DASHY_MODE"] = mode
	envMap["DASHY_SOURCE"] = source
	envMap["DASHY_SESSION_ID"] = a.sessionID()
	envMap["DASHY_HOME"] = a.home

	list := make([]string, 0, len(envMap))
	for k, v := range envMap {
		list = append(list, k+"="+v)
	}
	sort.Strings(list)
	return list
}

func (a *Agent) runDashyBlock(script string) string {
	dir := ""
	if a.Shell != nil {
		dir = a.Shell.Dir
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}

	timeout := dashyBlockTimeout
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	mode := a.mode
	if mode == "" {
		mode = "tui"
	}
	source := a.promptSource
	if source == "" {
		source = "startup"
	}

	env := a.dashyBlockEnv(mode, source)
	cmd := shell.Command(ctx, dir, env, script)

	stdoutBuf := newLimitWriter(DashyBlockMaxOutput)
	stderrBuf := newLimitWriter(DashyBlockMaxOutput)
	cmd.Stdout = stdoutBuf
	cmd.Stderr = stderrBuf

	startErr := cmd.Start()
	if startErr != nil {
		return formatDashyBlockFailure(startErr.Error(), stderrBuf.String())
	}

	waitErr := cmd.Wait()
	if ctx.Err() == context.DeadlineExceeded {
		reason := fmt.Sprintf("timed out after %s", timeout)
		return formatDashyBlockFailure(reason, stderrBuf.String())
	}
	if waitErr != nil {
		return formatDashyBlockFailure(waitErr.Error(), stderrBuf.String())
	}

	return strings.TrimRight(stdoutBuf.String(), "\r\n")
}

func (a *Agent) processAgentsFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	blocks := parseDashyBlocks(data)
	if len(blocks) == 0 {
		return string(data), nil
	}
	repls := make([]string, len(blocks))
	for i, b := range blocks {
		repls[i] = a.runDashyBlock(b.script)
	}
	return substituteBlocks(data, blocks, repls), nil
}

// AssemblePrompt gathers AGENTS.md files, runs dashy blocks, and formats the system prompt.
func (a *Agent) AssemblePrompt() string {
	return a.assemblePrompt()
}

func (a *Agent) assemblePrompt() string {
	cwd := ""
	if a.Shell != nil {
		cwd = a.Shell.Dir
	}
	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	var files []string

	// 1. Global file
	if !a.files.NoGlobal && a.home != "" {
		globalPath := filepath.Clean(filepath.Join(a.home, "AGENTS.md"))
		if st, err := os.Stat(globalPath); err == nil && !st.IsDir() {
			files = append(files, globalPath)
		}
	}

	// 2. Local file
	if a.files.Config != "" {
		p := a.files.Config
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if len(files) == 0 || files[0] != p {
				files = append(files, p)
			}
		}
	} else {
		p := filepath.Clean(filepath.Join(cwd, "AGENTS.md"))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			if len(files) == 0 || files[0] != p {
				files = append(files, p)
			}
		}
	}

	if len(files) == 0 {
		return ""
	}

	var sections []string
	for _, f := range files {
		content, err := a.processAgentsFile(f)
		if err != nil {
			continue
		}
		sections = append(sections, fmt.Sprintf("# %s\n%s", f, content))
	}

	if len(sections) == 0 {
		return ""
	}

	var sb strings.Builder
	for i, sec := range sections {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(strings.TrimRight(sec, "\r\n"))
	}
	if sb.Len() > 0 {
		sb.WriteString("\n")
	}
	return sb.String()
}
