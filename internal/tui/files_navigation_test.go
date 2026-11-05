package tui

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func resolveFileCompletion(t *testing.T, m model) model {
	t.Helper()
	next, cmd := m.Update(fileDebounceMsg{debounceGen: m.fileDebounceGen, fileGen: m.fileGen})
	m = next.(model)
	if cmd != nil {
		next, _ = m.Update(cmd())
		m = next.(model)
	}
	return m
}

func TestFileCompletionExternalPaths(t *testing.T) {
	home := t.TempDir()
	cwd := filepath.Join(home, "project")
	if err := os.MkdirAll(filepath.Join(home, "my folder", "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(cwd, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "my folder", "file.txt"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	m := bareModel(t, cwd)
	for _, query := range []string{"~/my folder/", "../my folder/", filepath.ToSlash(home) + "/my folder/"} {
		m.input.SetValue(quotePath(query))
		m.input.CursorEnd()
		m.checkFileCompletion()
		m = resolveFileCompletion(t, m)
		if want := []string{query + "file.txt", query + "sub/"}; !reflect.DeepEqual(m.fileCompletions, want) {
			t.Fatalf("%q completions = %v, want %v", query, m.fileCompletions, want)
		}
	}
	for _, query := range []string{"~/", "~", "./", "/"} {
		m.input.SetValue("@" + query)
		m.input.CursorEnd()
		m.checkFileCompletion()
		m = resolveFileCompletion(t, m)
		paths, err := scanProjectFiles(context.Background(), fileCompletionRoot(cwd, query))
		if err != nil {
			t.Fatal(err)
		}
		if want := filterFileCompletions(paths, query); !reflect.DeepEqual(m.fileCompletions, want) {
			t.Fatalf("%q: %v, want %v", query, m.fileCompletions, want)
		}
	}
	if root := fileCompletionRoot(cwd, "/"); root != "/" {
		t.Fatalf("absolute root: %q", root)
	}
	if root := fileCompletionRoot(cwd, "~"); root != home {
		t.Fatalf("home: %q", root)
	}
}

func TestDirectoryEnterClosesTabDescends(t *testing.T) {
	for _, key := range []tea.KeyType{tea.KeyEnter, tea.KeyTab} {
		t.Run(key.String(), func(t *testing.T) {
			cwd := t.TempDir()
			os.MkdirAll(filepath.Join(cwd, "my folder", "nested"), 0755)
			os.WriteFile(filepath.Join(cwd, "my folder", "child.txt"), nil, 0644)
			m := bareModel(t, cwd)
			m.input.SetValue("@my")
			m.input.CursorEnd()
			m.checkFileCompletion()
			m = resolveFileCompletion(t, m)
			if !reflect.DeepEqual(m.fileCompletions, []string{"my folder/"}) {
				t.Fatalf("root children: %v", m.fileCompletions)
			}
			next, _ := m.Update(tea.KeyMsg{Type: key})
			m = next.(model)
			if got := m.input.Value(); got != "@\"my folder/\"" {
				t.Fatalf("accepted: %q", got)
			}
			if m.ctl.Status().Busy {
				t.Fatal("accept submitted the prompt")
			}
			if key == tea.KeyEnter {
				if m.hasFileCompletions() || m.fileLoading {
					t.Fatal("Enter must close completion")
				}
				if cmd := m.checkFileCompletion(); cmd != nil {
					t.Fatal("closed completion reopened")
				}
			} else {
				if !m.fileLoading {
					t.Fatal("Tab must start browsing directory")
				}
				m = resolveFileCompletion(t, m)
				want := []string{"my folder/child.txt", "my folder/nested/"}
				if !reflect.DeepEqual(m.fileCompletions, want) {
					t.Fatalf("children: %v", m.fileCompletions)
				}
				next, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
				m = next.(model)
				if m.input.Value() != "@\"my folder/child.txt\"" || m.hasFileCompletions() {
					t.Fatalf("child acceptance: %q", m.input.Value())
				}
			}
		})
	}
}

func TestFileScanCancellationAndHiddenFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, ".hidden"), nil, 0644)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := scanProjectFiles(ctx, dir); err == nil {
		t.Fatal("cancelled scan succeeded")
	}
	paths, err := scanProjectFiles(context.Background(), dir)
	if err != nil || !slices.Contains(paths, ".hidden") {
		t.Fatalf("hidden files: %v %v", paths, err)
	}
}
