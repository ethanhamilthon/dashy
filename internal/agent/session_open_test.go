package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSessionOpenRejectsInvalidTargetsWithoutChangingCurrentSession(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	cwd := t.TempDir()
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.message("user", "keep current session")
	if err = a.Save(); err != nil {
		t.Fatal(err)
	}
	path, records := a.HistoryPath, a.Records()
	if err = os.Mkdir(filepath.Join(a.sessionsDir, "directory.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(path, filepath.Join(a.sessionsDir, "symlink.jsonl")); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"../escape", "nested/id", `nested\id`, "missing", "directory", "symlink"} {
		t.Run(id, func(t *testing.T) {
			if err := a.Resume(id); err == nil {
				t.Fatal("invalid resume target accepted")
			}
			if a.HistoryPath != path || !reflect.DeepEqual(a.Records(), records) {
				t.Fatal("failed resume changed current session")
			}
			if opened, err := New(cwd, ConfigFiles{}, id); err == nil {
				opened.Close()
				t.Fatal("invalid startup target accepted")
			}
		})
	}
	// Direct opening must not depend on summaries of unrelated sessions.
	if err = os.WriteFile(filepath.Join(a.sessionsDir, "broken.jsonl"), []byte("invalid\n"), 0600); err != nil {
		t.Fatal(err)
	}
	id := strings.TrimSuffix(filepath.Base(path), ".jsonl")
	if second, err := New(cwd, ConfigFiles{}, id); err == nil {
		second.Close()
		t.Fatal("an owned session opened twice")
	}
	a.Close()
	opened, err := New(cwd, ConfigFiles{}, id)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	if !reflect.DeepEqual(opened.Records(), records) {
		t.Fatal("direct opening lost records")
	}
}
