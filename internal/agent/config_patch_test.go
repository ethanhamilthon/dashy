package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestLosslessSparsePatchPreservesExactBytes(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	// Setup a global config with an inherited secret
	globalConfig := `provider = "global-prov"
model = "global-model"

[providers.global-prov]
base_url = "https://global.example"
api_key = "SUPER_SECRET_GLOBAL_KEY"
`
	if err := os.WriteFile(filepath.Join(home, "dashy.toml"), []byte(globalConfig), 0600); err != nil {
		t.Fatal(err)
	}

	multilineSystem := `system = '''
# Fake comment inside system prompt
[fake_section]
model = "fake-model"
provider = "fake-provider"
[providers.fake]
api_key = "fake_key"
'''`

	initialProject := `# Top-level root comments preserved
# Second comment line

max_context_tokens = 50000 # unrelated root setting
provider = 'orig' # primary provider inline
model   =   'orig-m'   # inline model comment

` + multilineSystem + `

# UI Section comment
[ui]
max_code_lines = 42 # ui inline

# Hook comment
[hooks.build]
events = ["turn_end"]
command = 'cargo test'

[providers.orig]
base_url = "https://orig.example"
api_key = "orig-key"

# Trailing EOF comment
`
	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(projectPath, []byte(initialProject), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// 1. Select updates targeted model and inserts missing effort
	if err := a.Select("orig", "updated-model", "high"); err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	afterSelect, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	contentSelect := string(afterSelect)

	// Check untouched bytes retained exactly:
	// - Leading comments
	if !strings.HasPrefix(contentSelect, "# Top-level root comments preserved\n# Second comment line\n\nmax_context_tokens = 50000 # unrelated root setting\n") {
		t.Fatalf("leading comments or root setting altered: %s", contentSelect)
	}
	// - provider was unchanged and kept single quotes and inline comment
	if !strings.Contains(contentSelect, "provider = 'orig' # primary provider inline\n") {
		t.Fatalf("provider format or inline comment altered: %s", contentSelect)
	}
	// - model value replaced in place, preserving spacing, single-quote format, and inline comment
	if !strings.Contains(contentSelect, "model   =   'updated-model'   # inline model comment\n") {
		t.Fatalf("model spacing, quote format, or inline comment altered: %s", contentSelect)
	}
	// - effort inserted right after model line
	if !strings.Contains(contentSelect, "effort = \"high\"\n") {
		t.Fatalf("effort missing after Select: %s", contentSelect)
	}
	// - multiline system retained byte-for-byte
	if !strings.Contains(contentSelect, multilineSystem) {
		t.Fatalf("multiline system altered: %s", contentSelect)
	}
	// - [ui] and comment retained byte-for-byte
	if !strings.Contains(contentSelect, "# UI Section comment\n[ui]\nmax_code_lines = 42 # ui inline\n") {
		t.Fatalf("[ui] section altered: %s", contentSelect)
	}
	// - [hooks.build] and comment retained byte-for-byte
	if !strings.Contains(contentSelect, "# Hook comment\n[hooks.build]\nevents = [\"turn_end\"]\ncommand = 'cargo test'\n") {
		t.Fatalf("[hooks.build] section altered: %s", contentSelect)
	}
	// - [providers.orig] retained byte-for-byte
	if !strings.Contains(contentSelect, "[providers.orig]\nbase_url = \"https://orig.example\"\napi_key = \"orig-key\"\n") {
		t.Fatalf("[providers.orig] altered: %s", contentSelect)
	}
	// - Trailing comment retained byte-for-byte
	if !strings.HasSuffix(contentSelect, "# Trailing EOF comment\n") {
		t.Fatalf("trailing comment altered: %s", contentSelect)
	}

	// 2. AddProvider with dotted/quoted provider name
	if err := a.AddProvider("p.dotted.name", Provider{BaseURL: "https://dotted.example", APIKey: "dotted-key"}); err != nil {
		t.Fatalf("AddProvider failed: %v", err)
	}

	afterAdd, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	contentAdd := string(afterAdd)

	// The entire content from contentSelect should be retained byte-for-byte as a prefix
	if !strings.HasPrefix(contentAdd, contentSelect) {
		t.Fatalf("AddProvider altered existing bytes! got:\n%s\nwant prefix:\n%s", contentAdd, contentSelect)
	}
	// And the new table should be cleanly appended at the end with safe quotes
	expectedTable := `[providers."p.dotted.name"]
base_url = "https://dotted.example"
api_key = "dotted-key"
`
	if !strings.HasSuffix(contentAdd, expectedTable) {
		t.Fatalf("expected appended provider table:\n%s\ngot:\n%s", expectedTable, contentAdd)
	}

	// 3. Verify no global inherited secrets flattened into project config
	if strings.Contains(contentAdd, "SUPER_SECRET_GLOBAL_KEY") || strings.Contains(contentAdd, "global-prov") {
		t.Fatalf("global inherited secret was leaked into project config: %s", contentAdd)
	}

	// 4. Atomic failure rollback on invalid effort
	backup := contentAdd
	if err := a.Select("orig", "updated-model", "unsupported-effort-level"); err == nil {
		t.Fatal("expected error on unsupported effort")
	}
	afterRollback, _ := os.ReadFile(projectPath)
	if string(afterRollback) != backup {
		t.Fatalf("atomic failure did not restore file byte-for-byte:\ngot:\n%s\nwant:\n%s", afterRollback, backup)
	}
}

func TestLosslessSparsePatchCRLF(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	crlfDoc := "# Windows CRLF config\r\n" +
		"provider = \"orig\"\r\n" +
		"model = \"orig-m\"\r\n" +
		"\r\n" +
		"[ui]\r\n" +
		"max_code_lines = 5\r\n" +
		"\r\n" +
		"[providers.orig]\r\n" +
		"base_url = \"https://orig.example\"\r\n" +
		"api_key = \"orig-key\"\r\n"

	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(projectPath, []byte(crlfDoc), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.Select("orig", "crlf-model", "low"); err != nil {
		t.Fatal(err)
	}
	if err := a.AddProvider("p_crlf", Provider{BaseURL: "https://crlf.example", APIKeyEnv: "CRLF_KEY"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}

	// Verify all line endings are CRLF
	lines := bytes.Split(data, []byte("\r\n"))
	for i, line := range lines {
		if bytes.Contains(line, []byte("\n")) || bytes.Contains(line, []byte("\r")) {
			t.Fatalf("line %d contains bare LF or CR: %q", i, line)
		}
	}

	// Verify values and comments
	content := string(data)
	if !strings.Contains(content, "# Windows CRLF config\r\n") {
		t.Fatalf("leading CRLF comment lost: %s", content)
	}
	if !strings.Contains(content, "model = \"crlf-model\"\r\n") {
		t.Fatalf("model not patched with CRLF: %s", content)
	}
	if !strings.Contains(content, "effort = \"low\"\r\n") {
		t.Fatalf("effort not inserted with CRLF: %s", content)
	}
	if !strings.Contains(content, "[providers.p_crlf]\r\nbase_url = \"https://crlf.example\"\r\napi_key_env = \"CRLF_KEY\"\r\n") {
		t.Fatalf("provider not appended with CRLF: %s", content)
	}
}

func TestLosslessPatchInlineProviders(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	doc := `# Inline providers test
provider = "old"
model = "m1"
providers = { old = { base_url = "https://old.example", api_key = "old-k" } } # inline comment

[ui]
max_code_lines = 10
`
	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(projectPath, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.AddProvider("new.dotted", Provider{BaseURL: "https://new.example", APIKey: "new-k"}); err != nil {
		t.Fatalf("AddProvider on inline table failed: %v", err)
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Verify comments and unrelated sections preserved
	if !strings.Contains(content, "# Inline providers test") || !strings.Contains(content, "# inline comment") {
		t.Fatalf("comments lost: %s", content)
	}
	if !strings.Contains(content, "[ui]\nmax_code_lines = 10") {
		t.Fatalf("[ui] lost: %s", content)
	}

	// Verify old provider bytes retained and new provider inserted into inline table
	expectedInline := `providers = { old = { base_url = "https://old.example", api_key = "old-k" }, "new.dotted" = { base_url = "https://new.example", api_key = "new-k" } } # inline comment`
	if !strings.Contains(content, expectedInline) {
		t.Fatalf("expected inline table update:\n%s\ngot:\n%s", expectedInline, content)
	}

	// Verify the config unmarshals and merges properly
	var m map[string]any
	if err := toml.Unmarshal(data, &m); err != nil {
		t.Fatalf("patched inline config failed to unmarshal: %v", err)
	}
	provs := m["providers"].(map[string]any)
	if provs["old"] == nil || provs["new.dotted"] == nil {
		t.Fatalf("providers not found in unmarshaled map: %+v", provs)
	}
}

func TestLosslessPatchRootKeysAbsent(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	doc := `# Only header comments here

# UI Section
[ui]
max_code_lines = 10

[providers.prov_x]
base_url = "https://prov-x.example"
api_key = "k"
`
	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(projectPath, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	if err := a.Select("prov_x", "model-x", "medium"); err != nil {
		t.Fatalf("Select failed: %v", err)
	}

	data, err := os.ReadFile(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)

	// Verify comments preserved
	if !strings.Contains(content, "# Only header comments here") || !strings.Contains(content, "# UI Section") {
		t.Fatalf("comments lost: %s", content)
	}
	// Verify inserted root keys exist before [ui]
	if !strings.Contains(content, "provider = \"prov_x\"\nmodel = \"model-x\"\neffort = \"medium\"\n") {
		t.Fatalf("root keys not inserted properly: %s", content)
	}
	// Verify [ui] preserved
	if !strings.Contains(content, "[ui]\nmax_code_lines = 10\n") {
		t.Fatalf("[ui] altered: %s", content)
	}
}

func TestLosslessPatchExistingProviderRejected(t *testing.T) {
	home, cwd := t.TempDir(), t.TempDir()
	t.Setenv("DASHY_HOME", home)

	doc := `provider = "p1"
model = "m1"

[providers.p1]
base_url = "https://p1.example"
`
	projectPath := filepath.Join(cwd, "dashy.toml")
	if err := os.WriteFile(projectPath, []byte(doc), 0600); err != nil {
		t.Fatal(err)
	}

	a, err := New(cwd, ConfigFiles{})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	err = a.AddProvider("p1", Provider{BaseURL: "https://dup.example"})
	if err == nil {
		t.Fatal("expected error on duplicate provider")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("unexpected error message: %v", err)
	}

	data, _ := os.ReadFile(projectPath)
	if string(data) != doc {
		t.Fatalf("file was modified on rejected provider: %s", data)
	}
}

func TestLosslessPatchClaudeReproduction(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "dashy.toml")

	initial := `# my project config
model = 'a'
system = '''line [x]
model = "z"
'''

# UI settings
[ui]
max_code_lines = 5

# Hooks section
[hooks.t]
events = ["turn_end"]
command = 'make test'
`
	if err := os.WriteFile(cfgPath, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}

	if err := patchRootKeys(cfgPath, map[string]string{"model": "b"}); err != nil {
		t.Fatal(err)
	}

	res, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	expected := `# my project config
model = 'b'
system = '''line [x]
model = "z"
'''

# UI settings
[ui]
max_code_lines = 5

# Hooks section
[hooks.t]
events = ["turn_end"]
command = 'make test'
`
	if string(res) != expected {
		t.Fatalf("Claude review2 reproduction mismatch!\nGot:\n%s\nWant:\n%s", string(res), expected)
	}
}
