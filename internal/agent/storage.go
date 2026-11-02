package agent

import (
	"bufio"
	"bytes"
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
// line for the custom title.
type sessionMeta struct {
	Meta *struct {
		Title string `json:"title"`
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
	Model string
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
		summary := sessionSummary(filepath.Join(a.sessionsDir, e.Name()))
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

// sessionSummary reads the title and model of a session file. The title is the
// custom title if there is one, else the first line of the first user message.
func sessionSummary(path string) SessionSummary {
	var summary SessionSummary
	file, err := os.Open(path)
	if err != nil {
		return summary
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var meta sessionMeta
		if json.Unmarshal(line, &meta) == nil && meta.Meta != nil {
			if meta.Meta.Title != "" {
				summary.Title = meta.Meta.Title
				return summary
			}
			continue
		}
		if summary.Title == "" {
			for _, r := range RecordsForItem(line) {
				if r.Kind == "user" {
					summary.Title = FirstLine(r.Text)
					break
				}
			}
		}
	}
	return summary
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

// DefaultTitle is the first line of the first user message in records.
func DefaultTitle(records []Record) string {
	for _, r := range records {
		if r.Kind == "user" {
			if line := FirstLine(r.Text); line != "" {
				return line
			}
		}
	}
	return ""
}

func (a *Agent) sessionData() ([]byte, error) {
	var buffer bytes.Buffer
	if a.title != "" {
		meta, err := json.Marshal(map[string]any{"meta": map[string]string{"title": a.title}})
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

// readSession loads history and the custom title. A torn last line is dropped.
func (a *Agent) readSession() error {
	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		return err
	}
	lines := bytes.Split(data, []byte("\n"))
	if len(lines) > 0 && len(lines[len(lines)-1]) == 0 {
		lines = lines[:len(lines)-1]
	}
	var history []json.RawMessage
	var title string
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
			return fmt.Errorf("session line %d is not a JSON object", i+1)
		}
		if meta.Meta != nil {
			title = meta.Meta.Title
			continue
		}
		history = append(history, append(json.RawMessage(nil), line...))
	}
	a.History, a.title = history, title
	return nil
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
