package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestInstructionFileSelection(t *testing.T) {
	for _, tc := range []struct {
		name, config, want                                        string
		global, local, emptyLocal, missingConfig, directoryConfig bool
	}{
		{name: "none"},
		{name: "global fallback", global: true, want: "global"},
		{name: "local only", local: true, want: "local"},
		{name: "local overrides global", global: true, local: true, want: "local"},
		{name: "empty local overrides global", global: true, local: true, emptyLocal: true, want: "local"},
		{name: "relative config overrides both", global: true, local: true, config: "custom.md", want: "config"},
		{name: "absolute config overrides both", global: true, local: true, config: "absolute", want: "config"},
		{name: "missing config does not fall back", global: true, local: true, config: "missing.md", missingConfig: true},
		{name: "directory config is an error", global: true, local: true, config: "directory", directoryConfig: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, parent := t.TempDir(), t.TempDir()
			dir := filepath.Join(parent, "project")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(parent, "DASHY.md"), "parent must not be inherited")
			paths := map[string]string{"global": filepath.Join(home, "DASHY.md"), "local": filepath.Join(dir, "DASHY.md")}
			if tc.global {
				writeFile(t, paths["global"], "global")
			}
			if tc.local {
				content := "local"
				if tc.emptyLocal {
					content = ""
				}
				writeFile(t, paths["local"], content)
			}
			config := tc.config
			if config != "" {
				paths["config"] = filepath.Join(dir, config)
				if tc.directoryConfig {
					if err := os.Mkdir(paths["config"], 0700); err != nil {
						t.Fatal(err)
					}
				} else if !tc.missingConfig {
					writeFile(t, paths["config"], "config")
				}
				if config == "absolute" {
					config = paths["config"]
				}
			}
			got, err := instructionFiles(home, dir, Options{Config: config})
			if tc.missingConfig || tc.directoryConfig {
				if err == nil {
					t.Fatal("invalid config was accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("unexpected instructions: %v", got)
				}
			} else if len(got) != 1 || got[0] != paths[tc.want] {
				t.Fatalf("files = %v, want only %s", got, paths[tc.want])
			}
		})
	}
}

func TestOverriddenGlobalBlocksDoNotRun(t *testing.T) {
	a := newTestAgentWith(t, &fakeProvider{}, Options{}, func(home, dir string) {
		writeFile(t, filepath.Join(home, "DASHY.md"), "```dashy\ntouch global-ran\n```\n")
		writeFile(t, filepath.Join(dir, "DASHY.md"), "local")
	})
	if _, err := a.instructions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "global-ran")); !os.IsNotExist(err) {
		t.Fatalf("overridden global block ran: %v", err)
	}
	if err := os.Remove(filepath.Join(a.Shell.Dir, "DASHY.md")); err != nil {
		t.Fatal(err)
	}
	if err := a.Reload(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.Shell.Dir, "global-ran")); err != nil {
		t.Fatalf("reload did not select global fallback: %v", err)
	}
}
