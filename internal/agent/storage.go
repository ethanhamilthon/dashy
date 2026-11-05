package agent

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// openProject selects the project of dir and opens or creates the session.
func (a *Agent) openProject(dir string) error {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	a.Shell.Dir, a.project = absolute, absolute
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
	var sel selection
	err := a.db.QueryRow(`SELECT title, provider, model, effort FROM sessions WHERE id = ? AND project = ?`, id, a.project).
		Scan(&a.title, &sel.Provider, &sel.Model, &sel.Effort)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session not found in this project: %s", id)
	}
	if err != nil {
		return err
	}
	a.id, a.stored = id, true
	a.fixSelection(sel)
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
	a.id = time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(id[:])
	a.itemMeta = nil
	a.fixSelection(selection{})
	return a.acquire()
}

// readSession loads the history items of the session.
func (a *Agent) readSession() error {
	rows, err := a.db.Query(`SELECT data, meta FROM items WHERE session = ? ORDER BY seq`, a.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	a.History, a.itemMeta = nil, nil
	for rows.Next() {
		var item []byte
		var meta string
		if err := rows.Scan(&item, &meta); err != nil {
			return err
		}
		a.History = append(a.History, json.RawMessage(item))
		var m Metadata
		if meta != "" && json.Unmarshal([]byte(meta), &m) == nil {
			a.setItemMeta(item, m)
		}
	}
	a.saved = len(a.History)
	return rows.Err()
}

// Save stores the session row and the history items added since the last save.
func (a *Agent) Save() error {
	// Read before the transaction: update holds cfg.mu while it writes the database.
	a.cfg.mu.Lock()
	sel := a.cfg.sel
	a.cfg.mu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO sessions (id, project, title, first, updated, provider, model, effort) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET title = excluded.title, first = excluded.first, updated = excluded.updated,
		provider = excluded.provider, model = excluded.model, effort = excluded.effort`,
		a.id, a.project, a.title, a.DefaultTitle(), time.Now().UnixNano(), sel.Provider, sel.Model, sel.Effort); err != nil {
		return err
	}
	start := min(a.saved, len(a.History))
	if _, err := tx.Exec(`DELETE FROM items WHERE session = ? AND seq >= ?`, a.id, start); err != nil {
		return err
	}
	for i := start; i < len(a.History); i++ {
		meta := ""
		if m := a.itemMeta[string(a.History[i])]; m != nil {
			data, _ := json.Marshal(m)
			meta = string(data)
		}
		if _, err := tx.Exec(`INSERT INTO items (session, seq, data, meta) VALUES (?, ?, ?, ?)`, a.id, i, string(a.History[i]), meta); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	a.saved, a.stored = len(a.History), true
	return nil
}

// SessionSummary is the /resume view of a saved session.
type SessionSummary struct {
	ID      string
	Title   string
	Updated time.Time
}

// Sessions returns saved sessions of this project, most recently updated first.
// The title is the custom one, else the first line of the first user message.
func (a *Agent) Sessions() ([]SessionSummary, error) {
	rows, err := a.db.Query(`SELECT id, CASE title WHEN '' THEN first ELSE title END, updated
		FROM sessions WHERE project = ? ORDER BY updated DESC, id DESC`, a.project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var updated int64
		if err := rows.Scan(&s.ID, &s.Title, &updated); err != nil {
			return nil, err
		}
		s.Updated = time.Unix(0, updated)
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
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

// DefaultTitle is the first line of the first user message.
func (a *Agent) DefaultTitle() string {
	return firstUserLine(a.History)
}
