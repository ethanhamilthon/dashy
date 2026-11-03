package agent

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Session files hold one history item per line plus at most one {"meta":{...}}
// line: the custom title, and the default title saved by compaction.
type sessionMeta struct {
	Meta *struct {
		Title        string `json:"title"`
		DefaultTitle string `json:"default_title"`
	} `json:"meta"`
}

// openProject selects the sessions directory of dir and opens or creates the session.
func (a *Agent) openProject(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	a.Shell.Dir = absolute
	hash := sha256.Sum256([]byte(absolute))
	a.sessionsDir = filepath.Join(a.home, "projects", hex.EncodeToString(hash[:]))
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		return err
	}
	if a.opts.Resume == "" {
		return a.createSession()
	}
	a.promptSource = "resume"
	return a.openSession(a.opts.Resume)
}

func (a *Agent) openSession(id string) error {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid session ID: %s", id)
	}
	path := filepath.Join(a.sessionsDir, id+".jsonl")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || (err == nil && !info.Mode().IsRegular()) {
		return fmt.Errorf("session not found in this project: %s", id)
	}
	if err != nil {
		return err
	}
	a.HistoryPath = path
	if err := a.acquire(); err != nil {
		return err
	}
	if err := a.readSession(); err != nil {
		a.release()
		return err
	}
	return nil
}

func (a *Agent) createSession() error {
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(id[:]) + ".jsonl"
	a.HistoryPath = filepath.Join(a.sessionsDir, name)
	return a.acquire()
}

// SessionSummary is the /resume view of a saved session.
type SessionSummary struct {
	ID    string
	Title string
	Mod   time.Time
}

// Sessions returns saved sessions of this project, most recently updated first.
func (a *Agent) Sessions() ([]SessionSummary, error) {
	entries, err := os.ReadDir(a.sessionsDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sessions []SessionSummary
	for _, e := range entries {
		if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		summary, err := sessionSummary(filepath.Join(a.sessionsDir, e.Name()))
		if err != nil {
			return nil, err
		}
		summary.ID, summary.Mod = strings.TrimSuffix(e.Name(), ".jsonl"), info.ModTime()
		sessions = append(sessions, summary)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].Mod.Equal(sessions[j].Mod) {
			return sessions[i].ID > sessions[j].ID
		}
		return sessions[i].Mod.After(sessions[j].Mod)
	})
	return sessions, nil
}

// sessionSummary reads the title of a session file: the custom title if there is
// one, else the default title, else the first line of the first user message.
// It uses the same reader as resume, so both show the same title.
func sessionSummary(path string) (SessionSummary, error) {
	history, custom, defaultTitle, err := readSessionFile(path)
	if err != nil {
		return SessionSummary{}, err
	}
	return SessionSummary{Title: cmp.Or(custom, defaultTitle, firstUserLine(history))}, nil
}

// firstUserLine is the first non-empty line of the first user record of history.
func firstUserLine(history []json.RawMessage) string {
	for _, item := range history {
		for _, r := range RecordsForItem(item) {
			if line := FirstLine(r.Text); r.Kind == "user" && line != "" {
				return line
			}
		}
	}
	return ""
}

// CustomTitle is the title set by /rename, or "" when the default applies.
func (a *Agent) CustomTitle() string { return a.title }

// Rename sets the custom title; an empty name restores the default (the first
// user message). The change is saved at once for a saved session.
func (a *Agent) Rename(name string) error {
	a.title = strings.Join(strings.Fields(name), " ")
	if !a.HasSavedSession() {
		return nil
	}
	return a.Save()
}

// FirstLine is the first non-empty line of text, trimmed.
func FirstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return ""
}

// DefaultTitle is the first line of the first user message, or the value saved by compaction.
func (a *Agent) DefaultTitle() string {
	if a.defaultTitle != "" {
		return a.defaultTitle
	}
	for _, r := range a.Records() {
		if line := FirstLine(r.Text); r.Kind == "user" && line != "" {
			return line
		}
	}
	return ""
}

func (a *Agent) sessionData() ([]byte, error) {
	var buffer bytes.Buffer
	if a.title != "" || a.defaultTitle != "" {
		meta, err := json.Marshal(map[string]any{"meta": map[string]string{"title": a.title, "default_title": a.defaultTitle}})
		if err != nil {
			return nil, err
		}
		buffer.Write(meta)
		buffer.WriteByte('\n')
	}
	for _, item := range a.History {
		var compact bytes.Buffer
		if err := json.Compact(&compact, item); err != nil {
			return nil, err
		}
		buffer.Write(compact.Bytes())
		buffer.WriteByte('\n')
	}
	return buffer.Bytes(), nil
}

// readSession loads history and the custom title.
func (a *Agent) readSession() error {
	var err error
	a.History, a.title, a.defaultTitle, err = readSessionFile(a.HistoryPath)
	return err
}

// readSessionFile parses a session file into history items, the last custom title
// and the last default title. A torn last line is dropped. There is no line length
// limit: a valid item of any size must read the same for the list and for resume.
func readSessionFile(path string) (history []json.RawMessage, title, defaultTitle string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", "", err
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var meta sessionMeta
		if err := json.Unmarshal(line, &meta); err != nil || line[0] != '{' {
			if i == len(lines)-1 {
				break // torn last line
			}
			return nil, "", "", fmt.Errorf("%s: line %d is not a JSON object", path, i+1)
		}
		if meta.Meta != nil {
			title, defaultTitle = meta.Meta.Title, meta.Meta.DefaultTitle
			continue
		}
		history = append(history, append(json.RawMessage(nil), line...))
	}
	return history, title, defaultTitle, nil
}

func writePrivate(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if _, err = file.Write(data); err == nil {
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp, path)
}
