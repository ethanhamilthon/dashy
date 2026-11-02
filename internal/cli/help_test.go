package cli

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestHelpHandling(t *testing.T) {
	for _, arg := range []string{"--help", "-h", "help"} {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			var diag bytes.Buffer
			err := Run(context.Background(), []string{arg}, strings.NewReader(""), &out, &diag)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diag.Len() != 0 {
				t.Fatalf("expected diagnostics to be empty, got: %q", diag.String())
			}
			if out.String() != helpText {
				t.Fatalf("output mismatch for %s", arg)
			}
		})
	}

	t.Run("help as flag with options", func(t *testing.T) {
		var out bytes.Buffer
		var diag bytes.Buffer
		err := Run(context.Background(), []string{"--config", "AGENTS.md", "--help"}, strings.NewReader(""), &out, &diag)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if diag.Len() != 0 {
			t.Fatalf("expected diagnostics to be empty, got: %q", diag.String())
		}
		if out.String() != helpText {
			t.Fatalf("output mismatch")
		}
	})
}

func TestHelpMentionsEveryFlag(t *testing.T) {
	fs := flag.NewFlagSet("dashy", flag.ContinueOnError)
	setupFlags(fs)

	var flagNames []string
	fs.VisitAll(func(f *flag.Flag) {
		flagNames = append(flagNames, f.Name)
	})

	for _, name := range flagNames {
		// In help text, flags appear as -name or --name
		shortOption := "-" + name
		longOption := "--" + name
		if !strings.Contains(helpText, shortOption) && !strings.Contains(helpText, longOption) {
			t.Errorf("flag %q not found in help text", name)
		}
	}

	// Also verify standard help flags appear in help text
	for _, h := range []string{"--help", "-h"} {
		if !strings.Contains(helpText, h) {
			t.Errorf("help flag %q not found in help text", h)
		}
	}
}

func TestHelpMentionsEveryDashyEnvVar(t *testing.T) {
	// Find repo root from current working directory
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("could not find repo root containing go.mod")
		}
		root = parent
	}

	envPattern := regexp.MustCompile(`\bDASHY_[A-Z0-9_]+\b`)
	usedEnvVars := make(map[string]struct{})

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		matches := envPattern.FindAllString(string(data), -1)
		for _, m := range matches {
			usedEnvVars[m] = struct{}{}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	if len(usedEnvVars) == 0 {
		t.Fatal("no DASHY_* environment variables found in non-test source code")
	}

	for envVar := range usedEnvVars {
		if !strings.Contains(helpText, envVar) {
			t.Errorf("environment variable %q is used in code but missing from help text", envVar)
		}
	}
}

func TestHelpStructure(t *testing.T) {
	// Must have no ANSI escape sequences
	if strings.Contains(helpText, "\x1b") {
		t.Error("help text contains ANSI escape sequences")
	}

	// Plain-text file, about 70 lines
	lines := strings.Split(strings.TrimSpace(helpText), "\n")
	if len(lines) < 50 || len(lines) > 90 {
		t.Errorf("expected ~70 lines in help.txt, got %d lines", len(lines))
	}

	// Must mention OSC 7501
	if !strings.Contains(helpText, "OSC 7501") {
		t.Error("help text must mention OSC 7501")
	}
}
