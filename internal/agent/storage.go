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

type sessionMeta struct {
	Title string `json:"title,omitempty"`
}

type sessionMetaLine struct {
	Meta sessionMeta `json:"meta"`
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
	if id == "" || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
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
	if err := a.readSession(); err != nil {
		a.release()
		return err
	}
	return nil
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
	if !a.HasSavedSession() {
		return nil
	}
	return a.Save()
}

// sessionSummary reads the title (from meta or the first user message).
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
		var metaLine struct {
			Meta *struct {
				Title string `json:"title"`
			} `json:"meta"`
		}
		if json.Unmarshal(line, &metaLine) == nil && metaLine.Meta != nil {
			if metaLine.Meta.Title != "" {
				summary.Title = metaLine.Meta.Title
				return summary
			}
			continue
		}
		for _, r := range RecordsForItem(line) {
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
	if a.title != "" {
		meta, err := json.Marshal(sessionMetaLine{
			Meta: sessionMeta{
				Title: a.title,
			},
		})
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
		var metaLine struct {
			Meta *struct {
				Title string `json:"title"`
			} `json:"meta"`
		}
		if err := json.Unmarshal(line, &metaLine); err != nil || line[0] != '{' {
			if i == len(lines)-1 {
				// Torn last line tolerated: drop it.
				break
			}
			if err != nil {
				return fmt.Errorf("session line %d: %w", i+1, err)
			}
			return fmt.Errorf("session line %d: invalid JSON object", i+1)
		}
		if metaLine.Meta != nil {
			if metaLine.Meta.Title != "" {
				title = metaLine.Meta.Title
			}
			continue
		}
		history = append(history, append(json.RawMessage(nil), line...))
	}
	a.History = history
	a.title = title
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
	hash := sha256.Sum256([]byte(filepath.Clean(absolute)))
	return filepath.Join(home, "projects", hex.EncodeToString(hash[:])), nil
}
