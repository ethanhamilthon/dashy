package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquire takes the exclusive per-session lock and, outside print mode, opens the session socket.
func (a *Agent) acquire() error {
	lockPath := filepath.Join(a.sessionsDir, a.SessionID()+".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		file.Close()
		return fmt.Errorf("session is in use by another dashy process")
	}
	if err != nil {
		file.Close()
		return err
	}
	a.owner = file
	if !a.opts.Print {
		if err := a.listenNotifications(); err != nil {
			a.release()
			return err
		}
	}
	return nil
}

// release closes the socket, drops the lock and removes the lock file of an unsaved session.
func (a *Agent) release() {
	if a.sock != nil {
		a.sock.retire()
		_ = os.Remove(a.sock.path)
		a.sock = nil
	}
	if a.owner != nil {
		_ = syscall.Flock(int(a.owner.Fd()), syscall.LOCK_UN)
		_ = a.owner.Close()
		a.owner = nil
		if _, err := os.Stat(a.HistoryPath); os.IsNotExist(err) {
			_ = os.Remove(filepath.Join(a.sessionsDir, a.SessionID()+".lock"))
		}
	}
}

// HasSavedSession reports whether the session has history or a file on disk.
func (a *Agent) HasSavedSession() bool {
	_, err := os.Stat(a.HistoryPath)
	return len(a.History) > 0 || err == nil
}

// Close stops running shell calls and releases the session.
func (a *Agent) Close() { a.release() }

// NewSession and Resume are called only when no turn is running.
func (a *Agent) NewSession() error      { return a.changeSession("") }
func (a *Agent) Resume(id string) error { return a.changeSession(id) }

// changeSession switches to a new or saved session and re-reads settings and
// AGENTS.md files. Resuming the current session keeps its lock and history but
// selects settings and AGENTS.md files again, and assembles the prompt again.
func (a *Agent) changeSession(id string) error {
	if id != "" && id == a.SessionID() {
		if err := a.loadConfig(a.Shell.Dir); err != nil {
			return err
		}
		a.promptFrozen, a.promptSource = false, "resume"
		return nil
	}
	next := &Agent{opts: a.opts, home: a.home, sessionsDir: a.sessionsDir, Shell: a.Shell,
		promptSource: "new", slots: &imageSlots{}}
	if err := next.loadConfig(a.Shell.Dir); err != nil {
		return err
	}
	if id != "" {
		next.promptSource = "resume"
	}
	var err error
	if id == "" {
		err = next.createSession()
	} else {
		err = next.openSession(id)
	}
	if err != nil {
		next.release()
		return err
	}
	if a.HasSavedSession() {
		if err = a.Save(); err != nil {
			next.release()
			return err
		}
	}
	a.release()
	*a = *next
	return nil
}

func (a *Agent) mode() string {
	if a.opts.Print {
		return "print"
	}
	return "tui"
}

// shellEnv is exported to every bash tool call. API keys and settings are never exported.
func (a *Agent) shellEnv() map[string]string {
	env := map[string]string{"DASHY_SESSION_ID": a.SessionID()}
	if a.sock != nil {
		env["DASHY_ENDPOINT"] = a.sock.path
	}
	return env
}
