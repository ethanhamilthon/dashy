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
	"unicode"
)

type sessionLine struct {
	Selection *Selection      `json:"selection,omitempty"`
	Complete  bool            `json:"complete,omitempty"`
	Items     int             `json:"items,omitempty"`
	Records   int             `json:"records,omitempty"`
	Version   int             `json:"version,omitempty"`
	CWD       string          `json:"cwd,omitempty"`
	Title     string          `json:"title,omitempty"`
	Item      json.RawMessage `json:"item,omitempty"`
	Record    *Record         `json:"record,omitempty"`
	// Literal marks a record whose empty Text is its own, never elided from a derived item.
	Literal bool `json:"literal,omitempty"`
}

func (a *Agent) openProject(cwd string, target ...string) error {
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return err
	}
	a.Shell.Dir = filepath.Clean(absolute)
	a.sessionsDir, err = projectDirectory(cwd, a.home)
	if err != nil {
		return err
	}
	if len(target) == 0 || target[0] == "" || target[0] == "new" {
		return a.createSession()
	}
	return a.openSession(target[0])
}
func (a *Agent) openSession(id string) error {
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("invalid session ID: %s", id)
	}
	path := filepath.Join(a.sessionsDir, id+".jsonl")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) || err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("session not found in this project: %s", id)
	}
	if err != nil {
		return err
	}
	a.HistoryPath = path
	if err := a.acquire(); err != nil {
		return err
	}
	return a.readSession()
}
func (a *Agent) createSession() error {
	if err := os.MkdirAll(a.sessionsDir, 0700); err != nil {
		return err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	a.HistoryPath = filepath.Join(a.sessionsDir, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+hex.EncodeToString(id[:])+".jsonl")
	return a.acquire()
}

// SessionSummary is the resume picker view of a saved session.
type SessionSummary struct {
	ID    string
	Title string
	Model string
	Mod   time.Time
}

// Sessions returns saved sessions of this project, most recently updated first.
func (a *Agent) Sessions() ([]SessionSummary, error) {
	if a.sessionsDir == "" {
		return nil, nil
	}
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
		path := filepath.Join(a.sessionsDir, e.Name())
		summary := sessionSummary(path)
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

// Rename sets the session title shown by /resume; an empty name restores the
// default (the first user message). Unsent sessions keep it until first save.
func (a *Agent) Rename(name string) error {
	a.title = strings.Join(strings.Fields(name), " ")
	defer a.titleChanged()
	if !a.HasSavedSession() {
		return nil
	}
	return a.Save()
}

// sessionSummary reads the model selection and the title or first user message.
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
		var entry sessionLine
		if json.Unmarshal(scanner.Bytes(), &entry) != nil || entry.Complete {
			break
		}
		if entry.Selection != nil {
			summary.Model = entry.Selection.Model
		}
		if entry.Title != "" {
			summary.Title = entry.Title
			return summary
		}
		for _, r := range RecordsForItem(entry.Item) {
			if r.Kind == "user" {
				summary.Title = strings.Join(strings.Fields(r.Text), " ")
				return summary
			}
		}
	}
	return summary
}
func (a *Agent) sessionData() ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	if err := encoder.Encode(sessionLine{Version: 2, CWD: a.Shell.Dir, Title: a.title, Selection: a.selection()}); err != nil {
		return nil, err
	}
	var derived []Record
	for _, item := range a.History {
		if err := encoder.Encode(sessionLine{Item: item}); err != nil {
			return nil, err
		}
		derived = append(derived, RecordsForItem(item)...)
	}
	records := a.Records()
	for i, record := range records {
		literal := record.Text == ""
		if i < len(derived) && record.Kind == derived[i].Kind && record.Text == derived[i].Text {
			record.Text = ""
		}
		if err := encoder.Encode(sessionLine{Record: &record, Literal: literal}); err != nil {
			return nil, err
		}
	}
	if err := encoder.Encode(sessionLine{Complete: true, Items: len(a.History), Records: len(records)}); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}
func (a *Agent) readSession() error {
	data, err := os.ReadFile(a.HistoryPath)
	if err != nil {
		return err
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		return fmt.Errorf("incomplete session file: %s", a.HistoryPath)
	}
	lines := bytes.Split(bytes.TrimSuffix(data, []byte("\n")), []byte("\n"))
	var selection *Selection
	var title string
	var history []json.RawMessage
	var derived []Record
	records := []Record{}
	for i, line := range lines {
		var entry sessionLine
		if err = json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("session line %d: %w", i+1, err)
		}
		if i == 0 {
			selection, title = entry.Selection, entry.Title
			if entry.Version != 2 || entry.CWD != a.Shell.Dir {
				return fmt.Errorf("unsupported session version or project mismatch")
			}
		} else if entry.Complete {
			if i != len(lines)-1 || entry.Items != len(history) || entry.Records != len(records) {
				return fmt.Errorf("session checkpoint mismatch")
			}
		} else if i == len(lines)-1 {
			return fmt.Errorf("session checkpoint missing")
		} else if entry.Item != nil {
			history = append(history, entry.Item)
			derived = append(derived, RecordsForItem(entry.Item)...)
		} else if entry.Record != nil {
			r := *entry.Record
			if r.Text == "" && !entry.Literal && len(records) < len(derived) && r.Kind == derived[len(records)].Kind {
				r.Text = derived[len(records)].Text
			}
			records = append(records, r)
		} else {
			return fmt.Errorf("invalid session line %d", i+1)
		}
	}
	if len(lines) < 2 {
		return fmt.Errorf("session checkpoint missing")
	}
	if selection != nil {
		s := a.Settings()
		s.Provider, s.Model, s.Effort = selection.Provider, selection.Model, selection.Effort
		if err := validateSettings(s); err != nil {
			return fmt.Errorf("session selection: %w", err)
		}
		a.applySettings(s)
	}
	a.History, a.records, a.title = history, records, title
	return nil
}

func writePrivate(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".history-*")
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

func projectDirectory(cwd, home string) (string, error) {
	absolute, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	name := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' {
			return r
		}
		return '-'
	}, filepath.Base(absolute))
	name = string([]rune(name)[:min(40, len([]rune(name)))])
	hash := sha256.Sum256([]byte(filepath.Clean(absolute)))
	return filepath.Join(home, "sessions", fmt.Sprintf("%s-%x", name, hash[:8])), nil
}
