package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// lockPath is the flock file that marks a session as open in some dashy process.
func (a *Agent) lockPath() string { return filepath.Join(a.home, "locks", a.id+".lock") }

// acquire takes the exclusive per-session lock and opens the session socket. Every session has
// its own socket, a dashy -p sub-agent too, so tasks and view from its shell reach it.
func (a *Agent) acquire() error {
	if err := os.MkdirAll(filepath.Dir(a.lockPath()), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(a.lockPath(), os.O_CREATE|os.O_RDWR, 0600)
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
	a.tasks = a.newTasks()
	if err := a.listenNotifications(); err != nil {
		a.release()
		return err
	}
	// Tasks see the session endpoint, so dashy commands in them find their session.
	env := a.shellEnv()
	a.tasks.mu.Lock()
	a.tasks.env = env
	a.tasks.mu.Unlock()
	return nil
}

// release closes the socket, drops the lock and removes the lock file of an unsaved session.
// The database stays open: a session change hands it to the next session.
func (a *Agent) release() {
	a.StopNotifications()
	if a.tasks != nil {
		a.tasks.close()
		a.tasks = nil
	}
	if a.owner != nil {
		_ = syscall.Flock(int(a.owner.Fd()), syscall.LOCK_UN)
		_ = a.owner.Close()
		a.owner = nil
		if !a.stored {
			_ = os.Remove(a.lockPath())
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

// HasSavedSession reports whether the session has history or is stored.
func (a *Agent) HasSavedSession() bool { return len(a.History) > 0 || a.stored }

// Close releases the session (its socket and its lock) and closes the database.
func (a *Agent) Close() {
	a.release()
	if a.db != nil {
		a.db.Close()
		a.db = nil
	}
}

// NewSession and Resume are called only when no turn is running.
func (a *Agent) NewSession() error      { return a.changeSession("") }
func (a *Agent) Resume(id string) error { return a.changeSession(id) }

// Reload re-reads settings and appends a fresh instruction snapshot.
func (a *Agent) Reload() error {
	if err := a.loadConfig(a.Shell.Dir); err != nil {
		return err
	}
	previous, frozen := a.prompt, a.promptFrozen
	a.promptFrozen, a.promptSource = false, "reload"
	if _, err := a.instructions(context.Background()); err != nil {
		a.prompt, a.promptFrozen = previous, frozen
		return err
	}
	return nil
}

// changeSession switches to a new or saved session and re-reads settings and
// DASHY.md files. Resuming keeps the stored instruction snapshot.
func (a *Agent) changeSession(id string) error {
	if id != "" && id == a.SessionID() {
		if err := a.loadConfig(a.Shell.Dir); err != nil {
			return err
		}
		a.promptSource = "resume"
		_, err := a.instructions(context.Background())
		return err
	}
	next := &Agent{opts: a.opts, home: a.home, db: a.db, project: a.project, Shell: a.Shell,
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
