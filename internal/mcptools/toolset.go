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

// ExposedHosts returns the aliases the user has exposed to agents.
func (t *ToolToggler) ExposedHosts() []string {
	return t.st.ExposedHosts()
}

// SetExposedHost turns one configured alias on or off for agents, then shows
// or hides ssh_hosts to match. The host list is persisted either way.
func (t *ToolToggler) SetExposedHost(alias string, on bool) {
	cur := t.st.ExposedHosts()
	next := make([]string, 0, len(cur)+1)
	seen := false
	for _, a := range cur {
		if a == alias {
			seen = true
			if on {
				next = append(next, a)
			}
			continue
		}
		next = append(next, a)
	}
	if on && !seen {
		next = append(next, alias)
	}
	t.st.SetExposedHosts(next)
	t.alignSSHHostsTool()
}

// ClearExposedHosts turns every host off and hides ssh_hosts.
func (t *ToolToggler) ClearExposedHosts() {
	t.st.SetExposedHosts(nil)
	t.alignSSHHostsTool()
}

// alignSSHHostsTool makes the ssh_hosts tool visible exactly when at least
// one host is exposed.
func (t *ToolToggler) alignSSHHostsTool() {
	on := len(t.st.ExposedHosts()) > 0
	if t.Enabled(SSHHostsTool) == on {
		_ = t.st.Persist()
		return
	}
	t.Set(SSHHostsTool, on)
}

// Enabled reports whether name is registered and currently visible to MCP clients.
func (t *ToolToggler) Enabled(name string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if _, ok := t.stash[name]; !ok {
		return false
	}
	return !t.disabled[name]
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
