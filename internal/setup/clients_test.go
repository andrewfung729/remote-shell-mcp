package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestJSONMergeAddsToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	res, err := mergeJSONMCPServers(path, "remote-shell", "/bin/launch", nil, nil, "mcpServers", false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Action != "added" {
		t.Fatalf("expected action=added, got %q", res.Action)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse written file: %v", err)
	}
	servers := got["mcpServers"].(map[string]any)
	entry := servers["remote-shell"].(map[string]any)
	if entry["command"] != "/bin/launch" {
		t.Fatalf("command not written: %v", entry)
	}
}

func TestJSONMergePreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	// Existing config has user prefs and one other MCP server.
	existing := `{
  "theme": "dark",
  "mcpServers": {
    "other": {"command": "/bin/other"}
  }
}
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := mergeJSONMCPServers(path, "remote-shell", "/bin/launch", nil, nil, "mcpServers", false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	data, _ := os.ReadFile(path)
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	if got["theme"] != "dark" {
		t.Fatalf("preserved key lost: %v", got)
	}
	servers := got["mcpServers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("other server lost")
	}
	if _, ok := servers["remote-shell"]; !ok {
		t.Fatalf("new server not added")
	}
	// Backup should exist.
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
}

func TestJSONMergeIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	_, err := mergeJSONMCPServers(path, "x", "/bin/x", nil, nil, "mcpServers", false)
	if err != nil {
		t.Fatal(err)
	}
	res, err := mergeJSONMCPServers(path, "x", "/bin/x", nil, nil, "mcpServers", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "unchanged" {
		t.Fatalf("expected unchanged on second call, got %q", res.Action)
	}
}

func TestJSONMergeUpdatesOnChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	_, _ = mergeJSONMCPServers(path, "x", "/bin/old", nil, nil, "mcpServers", false)
	res, err := mergeJSONMCPServers(path, "x", "/bin/new", nil, nil, "mcpServers", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "updated" {
		t.Fatalf("expected updated, got %q", res.Action)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "/bin/new") {
		t.Fatalf("new command not written")
	}
}

func TestJSONMergeDryRunWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	res, err := mergeJSONMCPServers(path, "x", "/bin/x", nil, nil, "mcpServers", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "would-add" {
		t.Fatalf("expected would-add, got %q", res.Action)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("dry run created the file!")
	}
}

func TestJSONMergeRefusesBadFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := mergeJSONMCPServers(path, "x", "/bin/x", nil, nil, "mcpServers", false)
	if err == nil {
		t.Fatalf("expected error on invalid JSON file")
	}
	if !strings.Contains(err.Error(), "refusing to overwrite") {
		t.Fatalf("expected refusal in error message, got %v", err)
	}
}

func TestTOMLMergeAddsToEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	res, err := mergeTOMLMCPServers(path, "remote-shell", "/bin/launch", nil, nil, false)
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if res.Action != "added" {
		t.Fatalf("expected added, got %q", res.Action)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := toml.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	servers := got["mcp_servers"].(map[string]any)
	entry := servers["remote-shell"].(map[string]any)
	if entry["command"] != "/bin/launch" {
		t.Fatalf("command not in TOML: %v", entry)
	}
}

func TestTOMLMergePreservesOtherKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	existing := `model = "gpt-5"

[mcp_servers.other]
command = "/bin/other"
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := mergeTOMLMCPServers(path, "remote-shell", "/bin/launch", nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	var got map[string]any
	_ = toml.Unmarshal(data, &got)
	if got["model"] != "gpt-5" {
		t.Fatalf("preserved key lost: %v", got)
	}
	servers := got["mcp_servers"].(map[string]any)
	if _, ok := servers["other"]; !ok {
		t.Fatalf("other server lost")
	}
	if _, ok := servers["remote-shell"]; !ok {
		t.Fatalf("new server not added")
	}
}

// New-adapter coverage: every client we ship in AllClients() must return a
// usable ConfigPath without erroring, and its Install path must produce a
// valid file. A wrong ConfigPath used to silently fall back to garbage
// paths; this test catches that on the host it actually runs on.
func TestEveryClientConfigPathResolves(t *testing.T) {
	for _, c := range AllClients() {
		t.Run(c.Name(), func(t *testing.T) {
			p, err := c.ConfigPath()
			if err != nil {
				t.Fatalf("ConfigPath: %v", err)
			}
			if p == "" {
				t.Fatalf("empty ConfigPath")
			}
			if !filepath.IsAbs(p) {
				t.Fatalf("ConfigPath not absolute: %q", p)
			}
		})
	}
}

func TestZedUsesContextServersKey(t *testing.T) {
	// Zed differs from every other JSON client because it stores MCP servers
	// under "context_servers" rather than "mcpServers". A regression here
	// would silently produce settings.json files Zed ignores.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if _, err := mergeJSONMCPServers(path, "remote-shell", "/bin/launch", nil, nil, "context_servers", false); err != nil {
		t.Fatalf("merge: %v", err)
	}
	data, _ := os.ReadFile(path)
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := got["context_servers"]; !ok {
		t.Fatalf("expected context_servers key, got: %v", got)
	}
	if _, ok := got["mcpServers"]; ok {
		t.Fatalf("unexpected mcpServers key on Zed config")
	}
}

func TestContinueSkipsWhenOnlyYAMLPresent(t *testing.T) {
	// If a user has migrated to Continue's YAML config we should NOT silently
	// create a stray config.json that Continue ignores — we return an error
	// pointing them at the YAML file so they can edit it themselves.
	dir := t.TempDir()
	// Simulate Continue's config dir layout: only config.yaml present.
	yamlPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(yamlPath, []byte("# continue config\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Point Continue at our temp dir by overriding HOME (Continue.ConfigPath
	// reads it via os.UserHomeDir). We restore at the end so we don't poison
	// other tests in the package.
	origHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", origHome) })
	_ = os.Setenv("HOME", filepath.Dir(dir))
	if err := os.Mkdir(filepath.Join(filepath.Dir(dir), ".continue"), 0o700); err == nil {
		// We need ~/.continue/config.yaml relative to the fake HOME.
		_ = os.Rename(yamlPath, filepath.Join(filepath.Dir(dir), ".continue", "config.yaml"))
	}

	res, err := Continue{}.Install("x", "/bin/x", nil, nil, false)
	if err == nil {
		t.Fatalf("expected error when YAML-only, got result=%+v", res)
	}
	if !strings.Contains(err.Error(), "YAML") {
		t.Fatalf("error should mention YAML: %v", err)
	}
}

func TestTOMLMergeWithArgsAndEnv(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")

	args := []string{"-addr", "127.0.0.1:7800"}
	env := map[string]string{"REMOTE_SHELL_MCP_TOKEN": "/tmp/tok"}
	_, err := mergeTOMLMCPServers(path, "remote-shell", "/bin/launch", args, env, false)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	s := string(data)
	for _, want := range []string{"command = '/bin/launch'", "args =", "-addr", "127.0.0.1:7800", "REMOTE_SHELL_MCP_TOKEN"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q in:\n%s", want, s)
		}
	}
}
