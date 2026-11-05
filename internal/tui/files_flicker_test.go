package tui

import (
	"reflect"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestFileCompletionFiltersImmediatelyWhileTyping(t *testing.T) {
	for _, expired := range []bool{false, true} {
		m := bareModel(t, t.TempDir())
		m.fileIndexCwd = m.cwd
		m.fileIndexCache = []string{"main.go", "map.go", "other.go"}
		m.fileIndexCacheTime = time.Now()
		if expired {
			m.fileIndexCacheTime = time.Now().Add(-2 * fileIndexCacheTTL)
		}
		m.input.SetValue("@")
		m.input.CursorEnd()
		m.checkFileCompletion()
		for _, char := range "mai" {
			next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{char}})
			m = next.(model)
			if !m.hasFileCompletions() {
				t.Fatalf("list disappeared at %q (expired=%t)", m.input.Value(), expired)
			}
			if want := filterFileCompletions(m.fileIndexCache, m.input.Value()[1:]); !reflect.DeepEqual(m.fileCompletions, want) {
				t.Fatalf("filter was delayed: %v, want %v", m.fileCompletions, want)
			}
			if m.fileLoading != expired {
				t.Fatalf("loading=%t, expired=%t", m.fileLoading, expired)
			}
		}
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
		m = next.(model)
		if !reflect.DeepEqual(m.fileCompletions, []string{"main.go", "map.go"}) {
			t.Fatalf("backspace: %v", m.fileCompletions)
		}
		m.input.SetValue("@elsewhere/")
		m.input.CursorEnd()
		m.checkFileCompletion()
		if m.hasFileCompletions() || !m.fileLoading {
			t.Fatal("changing directory retained unrelated candidates")
		}
	}
}
