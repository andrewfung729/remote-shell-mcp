package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/pelletier/go-toml/v2"
)

// Client is one MCP-aware tool the setup command can register us with.
type Client interface {
	Name() string
	ConfigPath() (string, error)
	Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error)
}

type Result struct {
	Path     string // config file written
	Action   string // "added" | "updated" | "unchanged" | "would-add" | "would-update" | "skipped"
	Backup   string // path to backup of previous config, if we wrote one
	Snippet  string // a small diff-ish representation of the change
}

// ----------------------------------------------------------------------------
// Claude Code CLI — writes ~/.claude.json
// ----------------------------------------------------------------------------

type ClaudeCode struct{}

func (ClaudeCode) Name() string { return "Claude Code" }

func (ClaudeCode) ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude.json"), nil
}

func (c ClaudeCode) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	return mergeJSONMCPServers(path, name, command, args, env, "mcpServers", dryRun)
}

// ----------------------------------------------------------------------------
// Claude Desktop — writes claude_desktop_config.json in the per-OS app-data dir
// ----------------------------------------------------------------------------

type ClaudeDesktop struct{}

func (ClaudeDesktop) Name() string { return "Claude Desktop" }

func (ClaudeDesktop) ConfigPath() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "Claude", "claude_desktop_config.json"), nil
	default:
		// Linux: Claude Desktop is unofficial here; use the XDG-ish path the
		// community settled on.
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "Claude", "claude_desktop_config.json"), nil
	}
}

func (c ClaudeDesktop) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	return mergeJSONMCPServers(path, name, command, args, env, "mcpServers", dryRun)
}

// ----------------------------------------------------------------------------
// Codex CLI — writes ~/.codex/config.toml with [mcp_servers.<name>] section
// ----------------------------------------------------------------------------

type CodexCLI struct{}

func (CodexCLI) Name() string { return "Codex CLI" }

func (CodexCLI) ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "config.toml"), nil
}

func (c CodexCLI) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	return mergeTOMLMCPServers(path, name, command, args, env, dryRun)
}

// ----------------------------------------------------------------------------
// Cursor — writes ~/.cursor/mcp.json with {"mcpServers": {...}}
// ----------------------------------------------------------------------------

type Cursor struct{}

func (Cursor) Name() string { return "Cursor" }

func (Cursor) ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".cursor", "mcp.json"), nil
}

func (c Cursor) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	return mergeJSONMCPServers(path, name, command, args, env, "mcpServers", dryRun)
}

// ----------------------------------------------------------------------------
// Windsurf — writes ~/.codeium/windsurf/mcp_config.json with {"mcpServers": ...}
// ----------------------------------------------------------------------------

type Windsurf struct{}

func (Windsurf) Name() string { return "Windsurf" }

func (Windsurf) ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codeium", "windsurf", "mcp_config.json"), nil
}

func (c Windsurf) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	return mergeJSONMCPServers(path, name, command, args, env, "mcpServers", dryRun)
}

// ----------------------------------------------------------------------------
// Zed — adds to settings.json under "context_servers" (Zed's MCP key name).
// settings.json lives at the user-config-dir (XDG-ish on macOS too).
// ----------------------------------------------------------------------------

type Zed struct{}

func (Zed) Name() string { return "Zed" }

func (Zed) ConfigPath() (string, error) {
	// macOS and Linux both use ~/.config/zed/settings.json. Zed doesn't use
	// ~/Library/Application Support on Darwin even though most other macOS
	// apps do.
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "zed", "settings.json"), nil
}

func (z Zed) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := z.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	return mergeJSONMCPServers(path, name, command, args, env, "context_servers", dryRun)
}

// ----------------------------------------------------------------------------
// Continue.dev — ~/.continue/config.json with "mcpServers" (when JSON is in
// use). Recent Continue ships with config.yaml instead; if that's the only
// file present we skip and report it rather than fight two source formats.
// ----------------------------------------------------------------------------

type Continue struct{}

func (Continue) Name() string { return "Continue.dev" }

func (Continue) ConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".continue", "config.json"), nil
}

func (c Continue) Install(name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	path, err := c.ConfigPath()
	if err != nil {
		return Result{}, err
	}
	// If the YAML variant exists and JSON does not, the user's on the modern
	// Continue config format which we don't write to yet. Bail with a clear
	// message instead of creating a stray config.json that Continue ignores.
	yamlPath := filepath.Join(filepath.Dir(path), "config.yaml")
	if _, statJSON := os.Stat(path); errors.Is(statJSON, fs.ErrNotExist) {
		if _, statYAML := os.Stat(yamlPath); statYAML == nil {
			return Result{Path: yamlPath, Action: "skipped"},
				fmt.Errorf("Continue.dev uses YAML config (%s); add the entry by hand for now", yamlPath)
		}
	}
	return mergeJSONMCPServers(path, name, command, args, env, "mcpServers", dryRun)
}

// ----------------------------------------------------------------------------
// JSON merge — used by Claude Code + Desktop + Cursor + Windsurf + Zed + Continue
// ----------------------------------------------------------------------------

func mergeJSONMCPServers(path, name, command string, args []string, env map[string]string, key string, dryRun bool) (Result, error) {
	res := Result{Path: path}

	var root map[string]any
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		root = map[string]any{}
	case err != nil:
		return res, fmt.Errorf("read %s: %w", path, err)
	default:
		if len(data) == 0 {
			root = map[string]any{}
		} else if err := json.Unmarshal(data, &root); err != nil {
			return res, fmt.Errorf("parse %s: %w (file exists but is not valid JSON — refusing to overwrite)", path, err)
		}
	}

	servers, _ := root[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	entry := map[string]any{"command": command}
	if len(args) > 0 {
		entry["args"] = args
	}
	if len(env) > 0 {
		entry["env"] = env
	}

	existing, has := servers[name]
	action, dryAction := "added", "would-add"
	if has {
		if jsonEqual(existing, entry) {
			res.Action = "unchanged"
			res.Snippet = renderJSONEntry(name, entry)
			return res, nil
		}
		action, dryAction = "updated", "would-update"
	}

	if dryRun {
		res.Action = dryAction
		res.Snippet = renderJSONEntry(name, entry)
		return res, nil
	}

	servers[name] = entry
	root[key] = servers

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if backup, err := backupFile(path); err != nil {
		return res, err
	} else {
		res.Backup = backup
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return res, err
	}
	if err := atomicWrite(path, append(out, '\n'), 0o600); err != nil {
		return res, err
	}
	res.Action = action
	res.Snippet = renderJSONEntry(name, entry)
	return res, nil
}

func renderJSONEntry(name string, entry map[string]any) string {
	b, _ := json.MarshalIndent(map[string]any{name: entry}, "  ", "  ")
	return "  " + string(b)
}

func jsonEqual(a, b any) bool {
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return string(ab) == string(bb)
}

// ----------------------------------------------------------------------------
// TOML merge — used by Codex CLI
// ----------------------------------------------------------------------------

type codexEntry struct {
	Command string            `toml:"command"`
	Args    []string          `toml:"args,omitempty"`
	Env     map[string]string `toml:"env,omitempty"`
}

func mergeTOMLMCPServers(path, name, command string, args []string, env map[string]string, dryRun bool) (Result, error) {
	res := Result{Path: path}

	var root map[string]any
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		root = map[string]any{}
	case err != nil:
		return res, fmt.Errorf("read %s: %w", path, err)
	default:
		if len(data) == 0 {
			root = map[string]any{}
		} else if err := toml.Unmarshal(data, &root); err != nil {
			return res, fmt.Errorf("parse %s: %w (file exists but is not valid TOML)", path, err)
		}
	}

	servers, _ := root["mcp_servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}

	entry := map[string]any{"command": command}
	if len(args) > 0 {
		entry["args"] = toAnySlice(args)
	}
	if len(env) > 0 {
		entry["env"] = toAnyMap(env)
	}

	existing, has := servers[name]
	action, dryAction := "added", "would-add"
	if has {
		if tomlEqual(existing, entry) {
			res.Action = "unchanged"
			res.Snippet = renderTOMLEntry(name, entry)
			return res, nil
		}
		action, dryAction = "updated", "would-update"
	}

	if dryRun {
		res.Action = dryAction
		res.Snippet = renderTOMLEntry(name, entry)
		return res, nil
	}

	servers[name] = entry
	root["mcp_servers"] = servers

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return res, fmt.Errorf("create %s: %w", filepath.Dir(path), err)
	}
	if backup, err := backupFile(path); err != nil {
		return res, err
	} else {
		res.Backup = backup
	}

	out, err := toml.Marshal(root)
	if err != nil {
		return res, err
	}
	if err := atomicWrite(path, out, 0o600); err != nil {
		return res, err
	}
	res.Action = action
	res.Snippet = renderTOMLEntry(name, entry)
	return res, nil
}

func toAnySlice(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

func toAnyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func tomlEqual(a, b any) bool {
	ab, _ := toml.Marshal(a)
	bb, _ := toml.Marshal(b)
	return string(ab) == string(bb)
}

func renderTOMLEntry(name string, entry map[string]any) string {
	out, _ := toml.Marshal(map[string]any{"mcp_servers": map[string]any{name: entry}})
	return string(out)
}

// ----------------------------------------------------------------------------
// File helpers
// ----------------------------------------------------------------------------

func backupFile(path string) (string, error) {
	_, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	backup := path + ".bak"
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(backup, data, 0o600); err != nil {
		return "", err
	}
	return backup, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
