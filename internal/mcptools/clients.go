package mcptools

import (
	"context"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/server"
)

// ClientRegistry tracks live MCP clients by transport session id so the
// WebUI can show which client opened which shell. Entries are added on
// session register (initialize) and removed on unregister (DELETE or
// stream close) — the MCP transport owns the lifecycle, we just mirror it.
type ClientRegistry struct {
	mu      sync.Mutex
	clients map[string]*ClientInfo
}

// ClientInfo is what the WebUI shows about a connected MCP client.
type ClientInfo struct {
	SessionID   string    `json:"session_id"`
	Name        string    `json:"name"`
	Version     string    `json:"version,omitempty"`
	ConnectedAt time.Time `json:"connected_at"`
}

func NewClientRegistry() *ClientRegistry {
	return &ClientRegistry{clients: map[string]*ClientInfo{}}
}

// Hooks returns server hooks that keep the registry in sync with the MCP
// transport's session lifecycle.
func (r *ClientRegistry) Hooks() *server.Hooks {
	h := &server.Hooks{}
	h.AddOnRegisterSession(func(_ context.Context, s server.ClientSession) {
		ci := &ClientInfo{SessionID: s.SessionID(), ConnectedAt: time.Now()}
		if sci, ok := s.(server.SessionWithClientInfo); ok {
			impl := sci.GetClientInfo()
			ci.Name, ci.Version = impl.Name, impl.Version
		}
		r.mu.Lock()
		r.clients[s.SessionID()] = ci
		r.mu.Unlock()
	})
	h.AddOnUnregisterSession(func(_ context.Context, s server.ClientSession) {
		r.mu.Lock()
		delete(r.clients, s.SessionID())
		r.mu.Unlock()
	})
	return h
}

// Get returns the client for a transport session id, or nil.
func (r *ClientRegistry) Get(sessionID string) *ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.clients[sessionID]
}

// List returns all live clients.
func (r *ClientRegistry) List() []ClientInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ClientInfo, 0, len(r.clients))
	for _, c := range r.clients {
		out = append(out, *c)
	}
	return out
}
