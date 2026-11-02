package agent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

func (a *Agent) acquire() error {
	dir := a.sessionsDir
	if dir == "" {
		dir = filepath.Dir(a.HistoryPath)
	}
	lockPath := filepath.Join(dir, a.sessionID()+".lock")
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		file.Close()
		return fmt.Errorf("session is in use by another dashy process: %s", a.sessionID())
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

func (a *Agent) release() {
	if a.listener != nil {
		a.listener.Close()
		a.listener = nil
		os.Remove(a.HistoryPath + ".endpoint")
		os.RemoveAll(a.socketDir)
	}
	if a.owner != nil {
		_ = syscall.Flock(int(a.owner.Fd()), syscall.LOCK_UN)
		_ = a.owner.Close()
		a.owner = nil
		dir := a.sessionsDir
		if dir == "" {
			dir = filepath.Dir(a.HistoryPath)
		}
		lockPath := filepath.Join(dir, a.sessionID()+".lock")
		if _, err := os.Stat(a.HistoryPath); os.IsNotExist(err) {
			_ = os.Remove(lockPath)
		}
	}
}

func (a *Agent) HasSavedSession() bool {
	_, err := os.Stat(a.HistoryPath)
	return len(a.History) > 0 || err == nil
}

func (a *Agent) Close() { a.Shell.Close(); a.release() }

// Session changes are called only after the active turn has finished.
func (a *Agent) NewSession() error      { return a.changeSession("") }
func (a *Agent) Resume(id string) error { return a.changeSession(id) }

func (a *Agent) changeSession(id string) error {
	if id != "" && id == strings.TrimSuffix(filepath.Base(a.HistoryPath), ".jsonl") {
		return nil
	}
	c, err := loadConfiguration(a.home, a.Shell.Dir, a.files)
	if err != nil {
		return err
	}
	next := &Agent{
		files:         a.files,
		home:          a.home,
		sessionsDir:   a.sessionsDir,
		Shell:         a.Shell,
		version:       a.version,
		System:        c.system,
		LocalConfig:   c.local,
		Sources:       c.sources,
		primaryConfig: c.primary,
	}
	next.applySettings(c.settings)
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
	a.Close()
	*a = *next
	return nil
}
