package tui

import (
	"context"
	"dashy/internal/filelist"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
)

const (
	fileDebounceDelay = 75 * time.Millisecond
	fileIndexCacheTTL = 5 * time.Second
	fileScanTimeout   = 2 * time.Second
	fileMaxCandidates = 100
	fileMaxIndexed    = 20000
	fileMaxDepth      = 20
)

type fileDebounceMsg struct{ debounceGen, fileGen int }
type fileScanMsg struct {
	paths   []string
	cwd     string
	fileGen int
	err     error
}

type fileToken struct {
	start, end int
	query      string
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 32 || r == 127 }) >= 0
}

func isIgnored(p string) bool {
	for _, part := range strings.Split(strings.Trim(p, "/"), "/") {
		if part == ".git" || part == ".dashy" || part == "node_modules" {
			return true
		}
	}
	return false
}

func sanitizeLabel(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 32 || r == 127 || !unicode.IsPrint(r) {
			return '?'
		}
		return r
	}, s)
}

func quotePath(p string) string {
	if !strings.ContainsAny(p, " \t\n\"'\\") {
		return "@" + p
	}
	return "@\"" + strings.ReplaceAll(strings.ReplaceAll(p, "\\", "\\\\"), "\"", "\\\"") + "\""
}

func unescapeQuoted(s string) string {
	var sb strings.Builder
	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		if runes[i] == '\\' && i+1 < len(runes) {
			i++
		}
		sb.WriteRune(runes[i])
	}
	return sb.String()
}

func findFileToken(text string, cursor int) (fileToken, bool) {
	runes := []rune(text)
	if cursor < 0 || cursor > len(runes) {
		return fileToken{}, false
	}
	for at := cursor - 1; at >= 0 && runes[at] != '\n'; at-- {
		if runes[at] == '@' {
			return parseFileToken(runes, at, cursor)
		}
	}
	return fileToken{}, false
}

func parseFileToken(runes []rune, at, cursor int) (fileToken, bool) {
	if at > 0 && (unicode.IsLetter(runes[at-1]) || unicode.IsDigit(runes[at-1]) || strings.ContainsRune("._%+-\\/:", runes[at-1])) {
		return fileToken{}, false
	}
	lineStart := at
	for lineStart > 0 && runes[lineStart-1] != '\n' {
		lineStart--
	}
	lp := string(runes[lineStart:at])
	if sp := strings.LastIndexByte(lp, ' '); sp >= 0 {
		lp = lp[sp+1:]
	}
	if strings.Contains(lp, "://") {
		return fileToken{}, false
	}
	var tokenEnd int
	var query string
	if at+1 < len(runes) && (runes[at+1] == '"' || runes[at+1] == '\'') {
		quote := runes[at+1]
		qEnd := at + 2
		for qEnd < len(runes) && runes[qEnd] != '\n' {
			if runes[qEnd] == '\\' && qEnd+1 < len(runes) && runes[qEnd+1] != '\n' {
				qEnd += 2
				continue
			}
			if runes[qEnd] == quote {
				break
			}
			qEnd++
		}
		tokenEnd = qEnd
		if qEnd < len(runes) && runes[qEnd] == quote {
			tokenEnd++
		}
		if cursor < at || cursor > tokenEnd {
			return fileToken{}, false
		}
		if end := min(cursor, qEnd); end > at+2 {
			query = unescapeQuoted(string(runes[at+2 : end]))
		}
	} else {
		tokenEnd = at + 1
		for tokenEnd < len(runes) && !unicode.IsSpace(runes[tokenEnd]) && !strings.ContainsRune(")]}>\\\";,`", runes[tokenEnd]) {
			tokenEnd++
		}
		if cursor < at || cursor > tokenEnd {
			return fileToken{}, false
		}
		query = string(runes[at+1 : cursor])
	}
	if strings.ContainsRune(query, '@') || hasControl(query) {
		return fileToken{}, false
	}
	return fileToken{start: at, end: tokenEnd, query: query}, true
}

func scanProjectFiles(ctx context.Context, root string) ([]string, error) {
	paths, _, err := filelist.List(ctx, root, filelist.Options{Directories: true, LinkFiles: true, Hidden: true, Max: fileMaxIndexed, Depth: fileMaxDepth, Ignore: func(p string) bool { return hasControl(p) || isIgnored(p) }})
	return paths, err
}

func filterFileCompletions(indexed []string, query string) []string {
	if query == "" {
		return indexed[:min(fileMaxCandidates, len(indexed))]
	}
	qLower, hasSlash := strings.ToLower(query), strings.Contains(query, "/")
	var cat0, cat1 []string
	for _, p := range indexed {
		pLower := strings.ToLower(p)
		baseLower := strings.ToLower(filepath.Base(p))
		if strings.HasSuffix(p, "/") {
			baseLower += "/"
		}
		if (hasSlash && strings.HasPrefix(pLower, qLower)) || (!hasSlash && strings.HasPrefix(baseLower, qLower)) {
			cat0 = append(cat0, p)
		} else if strings.Contains(pLower, qLower) {
			cat1 = append(cat1, p)
		}
	}
	slices.Sort(cat0)
	slices.Sort(cat1)
	res := append(cat0, cat1...)
	return res[:min(fileMaxCandidates, len(res))]
}

func (m *model) dismissFileCompletion() {
	m.fileCompletions, m.fileLoading, m.fileSelected, m.fileGen = nil, false, 0, m.fileGen+1
	m.fileDismissed = true
	m.fileDismissedValue = m.input.Value()
	m.fileDismissedCursor = m.composerCursorOffset()
	if m.fileScanCancel != nil {
		m.fileScanCancel()
		m.fileScanCancel = nil
	}
}

func (m model) hasFileCompletions() bool {
	return !m.hideCompletion && len(m.fileCompletions) > 0 && m.menuKind == ""
}

func (m *model) checkFileCompletion() tea.Cmd {
	if m.menuKind != "" {
		m.dismissFileCompletion()
		return nil
	}
	if m.fileDismissed && m.input.Value() == m.fileDismissedValue && m.composerCursorOffset() == m.fileDismissedCursor {
		return nil
	}
	token, ok := findFileToken(m.input.Value(), m.composerCursorOffset())
	if !ok {
		if m.fileLoading || len(m.fileCompletions) > 0 {
			m.dismissFileCompletion()
		}
		return nil
	}
	m.fileDismissed = false
	m.fileGen++
	m.fileDebounceGen++
	m.fileLoading, m.hideCompletion = true, false
	m.fileTokenStart, m.fileTokenEnd = token.start, token.end
	dGen, fGen := m.fileDebounceGen, m.fileGen
	return tea.Tick(fileDebounceDelay, func(time.Time) tea.Msg {
		return fileDebounceMsg{debounceGen: dGen, fileGen: fGen}
	})
}

func (m *model) handleFileDebounce(msg fileDebounceMsg) (tea.Model, tea.Cmd) {
	if msg.debounceGen != m.fileDebounceGen || msg.fileGen != m.fileGen {
		return *m, nil
	}
	if m.fileDismissed && m.input.Value() == m.fileDismissedValue && m.composerCursorOffset() == m.fileDismissedCursor {
		return *m, nil
	}
	token, ok := findFileToken(m.input.Value(), m.composerCursorOffset())
	if !ok {
		m.dismissFileCompletion()
		return *m, nil
	}
	m.fileTokenStart, m.fileTokenEnd = token.start, token.end
	if len(m.fileIndexCache) > 0 && m.fileIndexCwd == m.cwd && time.Since(m.fileIndexCacheTime) < fileIndexCacheTTL {
		m.fileCompletions, m.fileSelected, m.fileLoading = filterFileCompletions(m.fileIndexCache, token.query), 0, false
		m.refresh(true)
		return *m, nil
	}
	if m.fileScanCancel != nil {
		m.fileScanCancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), fileScanTimeout)
	m.fileScanCancel = cancel
	root, fGen := m.cwd, m.fileGen
	return *m, func() tea.Msg {
		paths, err := scanProjectFiles(ctx, root)
		return fileScanMsg{paths: paths, cwd: root, fileGen: fGen, err: err}
	}
}

func (m *model) handleFileScan(msg fileScanMsg) (tea.Model, tea.Cmd) {
	if msg.err == nil && len(msg.paths) > 0 {
		m.fileIndexCache, m.fileIndexCwd, m.fileIndexCacheTime = msg.paths, msg.cwd, time.Now()
	}
	if msg.fileGen != m.fileGen {
		return *m, nil
	}
	m.fileLoading = false
	if m.fileDismissed && m.input.Value() == m.fileDismissedValue && m.composerCursorOffset() == m.fileDismissedCursor {
		m.fileCompletions = nil
		m.refresh(true)
		return *m, nil
	}
	token, ok := findFileToken(m.input.Value(), m.composerCursorOffset())
	if !ok || msg.err != nil {
		m.fileCompletions = nil
	} else {
		m.fileTokenStart, m.fileTokenEnd = token.start, token.end
		m.fileCompletions = filterFileCompletions(msg.paths, token.query)
		m.fileSelected = 0
	}
	m.refresh(true)
	return *m, nil
}

func (m *model) acceptFileCompletion() tea.Cmd {
	if len(m.fileCompletions) == 0 {
		return nil
	}
	selected := m.fileCompletions[m.fileSelected%len(m.fileCompletions)]
	isDir, rep := strings.HasSuffix(selected, "/"), quotePath(selected)
	runes := []rune(m.input.Value())
	start, end := max(0, min(m.fileTokenStart, len(runes))), max(0, min(m.fileTokenEnd, len(runes)))
	m.input.SetValue(string(runes[:start]) + rep)
	cursor := len([]rune(m.input.Value()))
	if isDir && (strings.HasSuffix(rep, "\"") || strings.HasSuffix(rep, "'")) {
		cursor = max(0, cursor-1)
	}
	m.input.SetValue(m.input.Value() + string(runes[min(end, len(runes)):]))
	cmd := m.setCursorOffset(cursor)
	if isDir {
		m.fileDismissed = false
		m.fileSelected, m.fileLoading, m.hideCompletion, m.fileGen = 0, false, false, m.fileGen+1
		if token, ok := findFileToken(m.input.Value(), cursor); ok {
			m.fileTokenStart, m.fileTokenEnd = token.start, token.end
			if len(m.fileIndexCache) > 0 && m.fileIndexCwd == m.cwd && time.Since(m.fileIndexCacheTime) < fileIndexCacheTTL {
				m.fileCompletions = filterFileCompletions(m.fileIndexCache, token.query)
			}
		}
	} else {
		m.dismissFileCompletion()
	}
	return cmd
}
