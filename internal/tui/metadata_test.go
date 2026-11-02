package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"dashy/internal/agent"
)

func TestShortPathUsesTildeForHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		t.Skip("no usable home directory")
	}
	cases := map[string]string{
		home:                     "~",
		filepath.Join(home, "p"): "~" + string(filepath.Separator) + "p",
		home + "x/y":             home + "x/y",
		"/tmp/elsewhere":         "/tmp/elsewhere",
	}
	for path, want := range cases {
		if got := shortPath(path); got != want {
			t.Errorf("shortPath(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestBottomBarShortensWorkingDirectory(t *testing.T) {
	t.Setenv("DASHY_HOME", t.TempDir())
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == string(filepath.Separator) {
		t.Skip("no usable home directory")
	}
	a, err := agent.New(t.TempDir(), agent.ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(a, filepath.Join(home, "work", "demo"))
	m.width = 120
	if bar := m.bottomBar(); !strings.Contains(bar, "~/work/demo") {
		t.Fatalf("status bar does not show a shortened path: %q", bar)
	}
}

func TestFormatDurationAndTokens(t *testing.T) {
	if d := formatDuration(450 * time.Millisecond); d != "450ms" {
		t.Fatalf("formatDuration 450ms got %q", d)
	}
	if d := formatDuration(1500 * time.Millisecond); d != "1.5s" {
		t.Fatalf("formatDuration 1.5s got %q", d)
	}
	if tok := formatTokens(500); tok != "500" {
		t.Fatalf("formatTokens 500 got %q", tok)
	}
	if tok := formatTokens(12500); tok != "12.5k" {
		t.Fatalf("formatTokens 12500 got %q", tok)
	}
	if tok := formatTokens(2500000); tok != "2.5m" {
		t.Fatalf("formatTokens 2500000 got %q", tok)
	}
}

func TestFormatContext(t *testing.T) {
	if c := formatContext(500); c != "500" {
		t.Fatalf("formatContext 500 got %q", c)
	}
	if c := formatContext(1234); c != "1K" {
		t.Fatalf("formatContext 1234 got %q", c)
	}
	if c := formatContext(36679); c != "37K" {
		t.Fatalf("formatContext 36679 got %q", c)
	}
	if c := formatContext(1000000); c != "1M" {
		t.Fatalf("formatContext 1000000 got %q", c)
	}
	if c := formatContext(1500000); c != "1.5M" {
		t.Fatalf("formatContext 1500000 got %q", c)
	}
}

func TestFormatMetadata(t *testing.T) {
	meta := &agent.Metadata{
		Duration: 500 * time.Millisecond,
		Usage: &agent.Usage{
			Input:  1000,
			Output: 200,
			Cached: 500,
		},
	}
	s := formatMetadata(meta, "assistant")
	if !strings.Contains(s, "↑1.0k ↓200") || !strings.Contains(s, "500ms") || !strings.Contains(s, "cache 50%") {
		t.Fatalf("unexpected formatMetadata: %q", s)
	}
	if formatMetadata(meta, "user") != "" {
		t.Fatalf("user kind should have no metadata")
	}
}
