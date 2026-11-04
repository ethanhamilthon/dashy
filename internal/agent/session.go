package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// acquire takes the exclusive per-session lock and opens the session socket. Every session has
// its own socket, a dashy -p sub-agent too, so notify and view from its shell reach it.
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
	if err := a.listenNotifications(); err != nil {
		a.release()
		return err
	}
	return nil
}

// release closes the socket, drops the lock and removes the lock file of an unsaved session.
func (a *Agent) release() {
	a.StopNotifications()
	if a.owner != nil {
		_ = syscall.Flock(int(a.owner.Fd()), syscall.LOCK_UN)
		_ = a.owner.Close()
		a.owner = nil
		if _, err := os.Stat(a.HistoryPath); os.IsNotExist(err) {
			_ = os.Remove(filepath.Join(a.sessionsDir, a.SessionID()+".lock"))
		}
	}
}

// StopNotifications closes the session socket; later notify and view calls are refused.
// It reports whether a socket was open.
func (a *Agent) StopNotifications() bool {
	if a.sock == nil {
		return false
	}
	a.sock.retire()
	_ = os.Remove(a.sock.path)
	a.sock = nil
	return true
}

// HasSavedSession reports whether the session has history or a file on disk.
func (a *Agent) HasSavedSession() bool {
	_, err := os.Stat(a.HistoryPath)
	return len(a.History) > 0 || err == nil
}

// Close releases the session: its socket and its lock.
func (a *Agent) Close() { a.release() }

// NewSession and Resume are called only when no turn is running.
func (a *Agent) NewSession() error      { return a.changeSession("") }
func (a *Agent) Resume(id string) error { return a.changeSession(id) }

// Reload re-reads settings and DASHY.md files; the prompt is assembled again before the next request.
func (a *Agent) Reload() error {
	if err := a.loadConfig(a.Shell.Dir); err != nil {
		return err
	}
	a.promptFrozen, a.promptSource = false, "reload"
	return nil
}

// changeSession switches to a new or saved session and re-reads settings and
// DASHY.md files. Resuming the current session keeps its lock and history but
// selects settings and DASHY.md files again, and assembles the prompt again.
func (a *Agent) changeSession(id string) error {
	if id != "" && id == a.SessionID() {
		if err := a.loadConfig(a.Shell.Dir); err != nil {
			return err
		}
		a.promptFrozen, a.promptSource = false, "resume"
		return nil
	}
	next := &Agent{opts: a.opts, home: a.home, sessionsDir: a.sessionsDir, Shell: a.Shell,
		promptSource: "new", slots: &imageSlots{}, queue: &Steering{wake: a.queue.wake}}
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
