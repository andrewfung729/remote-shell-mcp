package mcptools

import (
	"sync"

	"github.com/mark3labs/mcp-go/server"
)

// ToolToggler owns the WebUI's per-tool on/off switches. A disabled tool is
// removed from the MCP server's live tool map (so tools/list omits it and
// tools/call fails) and can be re-added without a restart. The set of
// disabled names is persisted in the state file and re-applied on startup.
type ToolToggler struct {
	srv *server.MCPServer
	st  *State

	mu       sync.RWMutex
	disabled map[string]bool
	// stash holds the tool+handler pairs for disabled tools so they can be
	// re-added verbatim on enable.
	stash map[string]server.ServerTool
}

// NewToolToggler wraps a built MCP server. It snapshots every registered tool
// so any tool can be disabled and later re-added identically.
func NewToolToggler(srv *server.MCPServer, st *State) *ToolToggler {
	t := &ToolToggler{
		srv:      srv,
		st:       st,
		disabled: map[string]bool{},
		stash:    map[string]server.ServerTool{},
	}
	for name, tool := range srv.ListTools() {
		t.stash[name] = *tool
	}
	return t
}

// ApplyDisabled removes the named tools from the live server. Called once at
// startup with the persisted disabled list.
func (t *ToolToggler) ApplyDisabled(names []string) {
	t.mu.Lock()
	for _, n := range names {
		t.disabled[n] = true
	}
	t.mu.Unlock()
	if len(names) > 0 {
		t.srv.DeleteTools(names...)
	}
}

// Set enables or disables a single tool. Returns false if the tool name isn't
// one the server registered. The change is debounce-persisted and the MCP
// server emits tools/list_changed automatically.
func (t *ToolToggler) Set(name string, enabled bool) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	entry, ok := t.stash[name]
	if !ok {
		return false
	}
	if enabled {
		delete(t.disabled, name)
		t.srv.AddTools(entry)
	} else {
		t.disabled[name] = true
		t.srv.DeleteTools(name)
	}
	_ = t.st.Persist()
	return true
}

// DisabledNames returns the list of disabled tool names (for persistence).
func (t *ToolToggler) DisabledNames() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]string, 0, len(t.disabled))
	for n := range t.disabled {
		out = append(out, n)
	}
	return out
}

// ToolInfo is the WebUI's per-tool view.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

// List returns every registered tool with its enabled state — including
// disabled ones (they're off for MCP clients but the UI still shows the
// switch so you can turn them back on).
func (t *ToolToggler) List() []ToolInfo {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]ToolInfo, 0, len(t.stash))
	for name, entry := range t.stash {
		out = append(out, ToolInfo{
			Name:        name,
			Description: entry.Tool.Description,
			Enabled:     !t.disabled[name],
		})
	}
	return out
}
