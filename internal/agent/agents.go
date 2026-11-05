package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"dashy/internal/shell"
)

const (
	blockOutput     = 64 * 1024 // stdout: the start of the output is kept
	blockStderrTail = 4 * 1024  // the last stderr bytes, kept for a failure report only
	blockTailLines  = 5         // lines of that tail shown in a failure
)

var blockTimeout = 30 * time.Second // a variable only so tests can shorten it

type dashyBlock struct {
	start, end int // byte range replaced by the block output, fence lines included
	script     string
}

// parseDashyBlocks finds top-level fenced blocks whose info string is exactly "dashy".
// Blocks nested in other blocks or quotes, and other info strings, are ignored.
func parseDashyBlocks(source []byte) []dashyBlock {
	doc := goldmark.DefaultParser().Parse(text.NewReader(source))
	var blocks []dashyBlock
	for n := doc.FirstChild(); n != nil; n = n.NextSibling() {
		cb, ok := n.(*ast.FencedCodeBlock)
		if !ok || cb.Info == nil || string(cb.Info.Text(source)) != "dashy" {
			continue
		}
		start := cb.Info.Segment.Start
		for start > 0 && source[start-1] != '\n' {
			start--
		}
		opening := bytes.TrimLeft(source[start:cb.Info.Segment.Start], " ")
		if len(opening) == 0 {
			continue
		}
		fence := opening[0]
		fenceLen := bytes.IndexFunc(opening, func(r rune) bool { return r != rune(fence) })
		if fenceLen < 0 {
			fenceLen = len(opening)
		}
		// goldmark ends the body at the first closing fence, so the first matching line after it is that fence.
		end, pos := len(source), cb.Info.Segment.Stop
		if lines := cb.Lines(); lines.Len() > 0 {
			pos = lines.At(lines.Len() - 1).Stop
		}
		for pos < len(source) && end == len(source) {
			lineEnd := bytes.IndexByte(source[pos:], '\n')
			if lineEnd < 0 {
				lineEnd = len(source) - pos
			}
			line := source[pos : pos+lineEnd]
			if isClosingFence(line, fence, fenceLen) {
				end = pos + lineEnd
			}
			pos += lineEnd + 1
		}
		blocks = append(blocks, dashyBlock{start: start, end: end, script: string(cb.Lines().Value(source))})
	}
	return blocks
}

func isClosingFence(line []byte, fence byte, length int) bool {
	trimmed := bytes.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	count := bytes.IndexFunc(trimmed, func(r rune) bool { return r != rune(fence) })
	if count < 0 {
		count = len(trimmed)
	}
	return count >= length && len(bytes.Trim(trimmed[count:], " \t\r")) == 0
}

// blockOutputs captures one block run. Stdout keeps its first blockOutput bytes; stderr only
// keeps its end, for failure text, so it cannot crowd out stdout.
type blockOutputs struct {
	mu      sync.Mutex
	stdout  []byte
	errTail []byte // last blockStderrTail bytes of stderr
}

type blockWriter struct {
	o      *blockOutputs
	stderr bool
}

func (w blockWriter) Write(p []byte) (int, error) {
	w.o.mu.Lock()
	defer w.o.mu.Unlock()
	if w.stderr {
		w.o.errTail = append(w.o.errTail, p...)
		if len(w.o.errTail) > blockStderrTail {
			w.o.errTail = w.o.errTail[len(w.o.errTail)-blockStderrTail:]
		}
	}
	if !w.stderr {
		w.o.stdout = append(w.o.stdout, p[:min(len(p), blockOutput-len(w.o.stdout))]...)
	}
	return len(p), nil
}

// tail returns the last lines of stderr, or "" when there is none.
func (o *blockOutputs) tail() string {
	lines := strings.Split(strings.TrimSpace(string(o.errTail)), "\n")
	if len(lines) > blockTailLines {
		lines = lines[len(lines)-blockTailLines:]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func blockFailure(reason string, out *blockOutputs) string {
	text := fmt.Sprintf("[dashy block failed: %s]", reason)
	if tail := out.tail(); tail != "" {
		text += "\n" + tail
	}
	return text
}

// runBlock executes one dashy block. A block failure is returned as prompt text;
// only cancellation of the turn is an error, because the turn itself must stop.
func (a *Agent) runBlock(turn context.Context, script, mode, source string) (string, error) {
	execution := Execution{ID: fmt.Sprintf("instructions-%d", time.Now().UnixNano()), Kind: "instructions", Command: script, Started: time.Now()}
	a.observe(execution, "pending", shell.Result{})
	a.observe(execution, "in_progress", shell.Result{})
	ctx, cancel := context.WithTimeout(turn, blockTimeout)
	defer cancel()
	env := a.shellEnv()
	env["DASHY_MODE"], env["DASHY_SOURCE"], env["DASHY_HOME"] = mode, source, a.home
	out := &blockOutputs{}
	cmd := shell.Command(ctx, a.Shell.Dir, env, script)
	cmd.Stdout, cmd.Stderr = blockWriter{out, false}, blockWriter{out, true}
	var err error
	if err = cmd.Start(); err == nil {
		err = shell.Reap(cmd)
	}
	code := 0
	if err != nil {
		code = 1
	}
	if ctx.Err() != nil {
		code = 124
	}
	if turn.Err() != nil {
		code = 130
	}
	a.completed(execution, shell.Result{Output: string(out.stdout), ExitCode: code, Error: code != 0})
	if turn.Err() != nil {
		return "", turn.Err()
	}
	if ctx.Err() != nil {
		return blockFailure(fmt.Sprintf("timed out after %s", blockTimeout), out), nil
	}
	if err != nil {
		return blockFailure(err.Error(), out), nil
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	return strings.TrimRight(string(out.stdout), "\r\n"), nil
}

// instructions returns the system prompt. It is assembled lazily, right before the
// first request of a new session, then stored and reused until /reload.
func (a *Agent) instructions(ctx context.Context) (string, error) {
	if a.promptFrozen {
		return a.prompt, nil
	}
	prompt, err := a.assemblePrompt(ctx)
	if err != nil {
		return "", err
	}
	a.History = append(a.History, raw(map[string]string{"type": "instructions", "text": prompt}))
	if err := a.saveHistory(); err != nil {
		a.History = a.History[:len(a.History)-1]
		return "", err
	}
	a.prompt, a.promptFrozen = prompt, true
	return prompt, nil
}

// assemblePrompt builds the system prompt from the selected DASHY.md files.
// Each file is a "# <path>" header and its bytes verbatim, with dashy blocks
// replaced by their output; files are separated by one blank line. A selected
// file that cannot be read is an error, never a silently shorter prompt.
func (a *Agent) assemblePrompt(ctx context.Context) (string, error) {
	var out strings.Builder
	for i, path := range a.promptFiles {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read instructions: %w", err)
		}
		content, err := a.expandBlocks(ctx, data)
		if err != nil {
			return "", err
		}
		if i > 0 {
			out.WriteString(separator(out.String()))
		}
		out.WriteString("# " + path + "\n" + content)
	}
	return out.String(), nil
}

// separator returns the bytes that make exactly one blank line after text.
func separator(text string) string {
	if strings.HasSuffix(text, "\n") {
		return "\n"
	}
	return "\n\n"
}

func (a *Agent) expandBlocks(ctx context.Context, data []byte) (string, error) {
	blocks := parseDashyBlocks(data)
	var out strings.Builder
	last := 0
	for _, b := range blocks {
		replacement, err := a.runBlock(ctx, b.script, a.mode(), a.promptSource)
		if err != nil {
			return "", err
		}
		out.Write(data[last:b.start])
		out.WriteString(replacement)
		last = b.end
	}
	out.Write(data[last:])
	return out.String(), nil
}

// instructionFiles selects the DASHY.md files: the global one (unless NoGlobal),
// then --config FILE or ./DASHY.md. Missing optional files are skipped.
func instructionFiles(home, dir string, files Options) ([]string, error) {
	var paths []string
	if !files.NoGlobal && home != "" {
		if p := filepath.Join(home, "DASHY.md"); isFile(p) {
			paths = append(paths, p)
		}
	}
	if files.Config != "" {
		p := files.Config
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		p = filepath.Clean(p)
		if st, err := os.Stat(p); err != nil {
			return nil, fmt.Errorf("config %s: %w", p, err)
		} else if st.IsDir() {
			return nil, fmt.Errorf("config %s: is a directory", p)
		}
		return append(paths, p), nil
	}
	if p := filepath.Join(dir, "DASHY.md"); isFile(p) {
		paths = append(paths, p)
	}
	return paths, nil
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
