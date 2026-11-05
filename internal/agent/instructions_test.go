package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstructionSnapshotsPersistAndReload(t *testing.T) {
	fp := &fakeProvider{reply: func(int, string) string { return textReply("ok") }}
	a := newTestAgentWith(t, fp, Options{}, func(_, dir string) { writeFile(t, filepath.Join(dir, "DASHY.md"), "initial") })
	turn(t, a, "first")
	id, dir := a.id, a.Shell.Dir
	a.Close()
	writeFile(t, filepath.Join(dir, "DASHY.md"), "changed")
	b, err := New(dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !strings.Contains(b.prompt, "initial") {
		t.Fatal(b.prompt)
	}
	turn(t, b, "second")
	if err := b.Reload(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.prompt, "changed") {
		t.Fatal(b.prompt)
	}
	if err := os.Remove(filepath.Join(dir, "DASHY.md")); err != nil {
		t.Fatal(err)
	}
	if err := b.Resume(id); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.prompt, "changed") {
		t.Fatal("latest snapshot lost")
	}
	count := 0
	for _, item := range b.History {
		if _, ok := instructionSnapshot(item); ok {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("snapshots %d", count)
	}
	for _, body := range fp.requests() {
		var payload struct{ Input []json.RawMessage }
		if err := json.Unmarshal([]byte(body), &payload); err != nil {
			t.Fatal(err)
		}
		for _, item := range payload.Input {
			if _, ok := instructionSnapshot(item); ok {
				t.Fatal("snapshot sent as input")
			}
		}
	}
	for _, r := range b.Records() {
		if strings.Contains(r.Text, `"type":"instructions"`) {
			t.Fatal("snapshot visible")
		}
	}
}

func TestLegacySessionGetsOneSnapshot(t *testing.T) {
	a := newTestAgent(t, &fakeProvider{}, Options{})
	a.message("user", "legacy")
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	id, dir := a.id, a.Shell.Dir
	a.Close()
	b, err := New(dir, Options{Resume: id})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !b.promptFrozen || len(b.History) != 2 {
		t.Fatal("legacy snapshot missing")
	}
	if _, err := b.instructions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Resume(id); err != nil {
		t.Fatal(err)
	}
	if len(b.History) != 2 {
		t.Fatal("legacy snapshot duplicated")
	}
}
