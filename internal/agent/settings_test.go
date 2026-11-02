package agent

import (
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
)

func TestInvalidSettingsValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		s    Settings
	}{
		{"missing provider", Settings{Provider: "missing"}},
		{"bad reasoning_summary", Settings{Provider: "p", ReasoningSummary: "invalid", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}}},
		{"bad effort", Settings{Provider: "p", Effort: "oops", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}}},
		{"bad url", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "file:///tmp"}}}},
		{"conflicting api key", Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "https://example.com", APIKey: "k", APIKeyEnv: "E"}}}},
		{"zero shell timeout", Settings{Provider: "p", ShellTimeout: 0, ShellMaxTimeout: 600, Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}}},
		{"shell timeout exceeds max", Settings{Provider: "p", ShellTimeout: 200, ShellMaxTimeout: 100, Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSettings(tc.s); err == nil {
				t.Fatal("expected settings error")
			}
		})
	}
}

func TestRelativeHome(t *testing.T) {
	cwd := t.TempDir()
	t.Setenv("DASHY_HOME", "custom/.dashy")
	dir, err := configDir(cwd)
	if err != nil || dir != filepath.Join(cwd, "custom", ".dashy") {
		t.Fatalf("%s %v", dir, err)
	}
}

func TestSettingsSnapshotDoesNotMutateAgent(t *testing.T) {
	a := &Agent{}
	a.applySettings(Settings{Provider: "p", Model: "m", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}})
	snapshot := a.Settings()
	snapshot.Model = "changed"
	snapshot.Providers["p"] = Provider{BaseURL: "changed"}
	if a.Settings().Model != "m" || a.Settings().Providers["p"].BaseURL != "https://example.com" {
		t.Fatal("mutable settings exposed")
	}
}

func TestShellTimeoutBoundaryAndPerCallToolChecks(t *testing.T) {
	const maxDurSec = int64(math.MaxInt64 / 1000000000)

	base := Settings{
		Provider:        "p",
		Providers:       map[string]Provider{"p": {BaseURL: "https://example.com"}},
		ShellTimeout:    int(maxDurSec),
		ShellMaxTimeout: int(maxDurSec),
		ShellMaxOutput:  0,
	}
	if err := validateSettings(base); err != nil {
		t.Fatalf("boundary max duration should be valid, got: %v", err)
	}

	overflowTimeout := base
	overflowTimeout.ShellTimeout = int(maxDurSec) + 1
	overflowTimeout.ShellMaxTimeout = int(maxDurSec) + 1
	if err := validateSettings(overflowTimeout); err == nil {
		t.Fatal("expected duration overflow error for timeout exceeding maxDurSec")
	}

	overflowMax := base
	overflowMax.ShellTimeout = 120
	overflowMax.ShellMaxTimeout = int(maxDurSec) + 1
	if err := validateSettings(overflowMax); err == nil {
		t.Fatal("expected duration overflow error for max_timeout exceeding maxDurSec")
	}

	cwd := t.TempDir()
	home := t.TempDir()
	t.Setenv("DASHY_HOME", home)
	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.ApplySettings(Settings{
		Provider:        "p",
		Providers:       map[string]Provider{"p": {BaseURL: "https://example.com"}},
		ShellTimeout:    120,
		ShellMaxTimeout: 600,
		ShellMaxOutput:  1024,
	})

	res := a.runTool(t.Context(), "bash", fmt.Sprintf(`{"command":"echo hi","timeout":%d}`, math.MaxInt64))
	if !res.Error || !strings.Contains(res.Output, "timeout must be 1..600 seconds") {
		t.Fatalf("expected 1..600 timeout error on math.MaxInt64, got: %+v", res)
	}

	res = a.runTool(t.Context(), "bash", `{"command":"echo hi","timeout":0}`)
	if !res.Error || !strings.Contains(res.Output, "timeout must be 1..600 seconds") {
		t.Fatalf("expected 1..600 timeout error on 0, got: %+v", res)
	}

	res = a.runTool(t.Context(), "bash", `{"command":"echo hi","timeout":601}`)
	if !res.Error || !strings.Contains(res.Output, "timeout must be 1..600 seconds") {
		t.Fatalf("expected 1..600 timeout error on 601, got: %+v", res)
	}

	res = a.runTool(t.Context(), "bash", `{"command":"echo hi","max_output":-1}`)
	if !res.Error || !strings.Contains(res.Output, "max_output must be >= 0 bytes") {
		t.Fatalf("expected >= 0 bytes error on -1, got: %+v", res)
	}

	res = a.runTool(t.Context(), "bash", `{"command":"printf 'hello world'","max_output":0}`)
	if res.Error || res.Output != "hello world" {
		t.Fatalf("expected zerooutput / no truncation, got: %+v", res)
	}
}

func TestMaxFileBytesValidation(t *testing.T) {
	base := Settings{Provider: "p", Providers: map[string]Provider{"p": {BaseURL: "https://example.com"}}, ShellTimeout: 120, ShellMaxTimeout: 600}
	base.MaxFileBytes = -1
	if err := validateSettings(base); err == nil {
		t.Fatal("negative max_file_bytes accepted")
	}
	base.MaxFileBytes = 0
	if err := validateSettings(base); err != nil {
		t.Fatalf("zero (unlimited) rejected: %v", err)
	}
}
