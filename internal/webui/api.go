// Package webui serves the daemon's configuration/monitoring HTTP API and
// its single static page. Handlers call the SSH manager, state store and
// tool toggler directly — never through MCP tools — so disabling any MCP
// tool cannot lock the user out of this surface.
package webui

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/mcptools"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
	"github.com/jaenster/remote-shell-mcp/internal/state"
)

var startedAt = time.Now()

// Deps are the collaborators the API needs. All are the same live instances
// the MCP tools share, so the page always reflects real state.
type Deps struct {
	SSH       *sshx.Manager
	Store     *state.Store
	Toggler   *mcptools.ToolToggler
	Version   string
	BoundAddr string
	Log       *slog.Logger
}

// New returns an http.Handler serving the JSON API under /api and the
// embedded page at /. Mount it on the same mux as the MCP endpoint; the
// Host/Origin guard is applied by the caller around the whole mux.
func New(d *Deps) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/status", d.handleStatus)
	mux.HandleFunc("GET /api/sessions", d.handleSessions)
	mux.HandleFunc("POST /api/sessions/connect", d.handleConnect)
	mux.HandleFunc("POST /api/sessions/disconnect", d.handleDisconnect)
	mux.HandleFunc("POST /api/sessions/reconnect", d.handleReconnect)
	mux.HandleFunc("GET /api/tools", d.handleTools)
	mux.HandleFunc("POST /api/tools/set", d.handleToolSet)

	// Static page: everything else falls through to the embedded SPA.
	mux.Handle("/", pageHandler())

	return mux
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func decode(w http.ResponseWriter, r *http.Request, out any) bool {
	if err := json.NewDecoder(r.Body).Decode(out); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// --- status ---------------------------------------------------------------

type statusResp struct {
	Uptime    string `json:"uptime"`
	Version   string `json:"version"`
	Listen    string `json:"listen"`
	StatePath string `json:"state_path"`
}

func (d *Deps) handleStatus(w http.ResponseWriter, r *http.Request) {
	path := ""
	if d.Store != nil {
		path = d.Store.Path()
	}
	writeJSON(w, http.StatusOK, statusResp{
		Uptime:    time.Since(startedAt).Round(time.Second).String(),
		Version:   d.Version,
		Listen:    d.BoundAddr,
		StatePath: path,
	})
}

// --- sessions -------------------------------------------------------------

type shellView struct {
	ID  string `json:"id"`
	Age string `json:"age"`
}

type sessionView struct {
	ID            string      `json:"id"`
	State         string      `json:"state"`
	User          string      `json:"user"`
	Addresses     []string    `json:"addresses"`
	ActiveAddress string      `json:"active_address,omitempty"`
	Persistent    bool        `json:"persistent"`
	AutoReconnect bool        `json:"auto_reconnect"`
	ForwardAgent  bool        `json:"forward_agent"`
	LastError     string      `json:"last_error,omitempty"`
	Shells        []shellView `json:"shells"`
}

func (d *Deps) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions := d.SSH.Sessions()
	out := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		info := s.Info()
		shs := s.ListShells()
		shells := make([]shellView, 0, len(shs))
		for _, sh := range shs {
			shells = append(shells, shellView{ID: sh.ID, Age: time.Since(sh.OpenedAt).Round(time.Second).String()})
		}
		out = append(out, sessionView{
			ID:            info.ID,
			State:         info.State,
			User:          info.User,
			Addresses:     info.Addresses,
			ActiveAddress: info.ActiveAddress,
			Persistent:    info.Persistent,
			AutoReconnect: info.AutoReconnect,
			ForwardAgent:  s.Spec.ForwardAgent,
			LastError:     info.LastError,
			Shells:        shells,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// --- session mutations -----------------------------------------------------

type connectReq struct {
	ID   string           `json:"id"`
	Spec sshx.ConnectSpec `json:"spec"`
}

func (d *Deps) handleConnect(w http.ResponseWriter, r *http.Request) {
	var req connectReq
	if !decode(w, r, &req) {
		return
	}
	if req.ID == "" {
		writeErr(w, http.StatusBadRequest, "id is required")
		return
	}
	if _, err := d.SSH.Connect(req.ID, req.Spec); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	d.persist()
	writeJSON(w, http.StatusOK, map[string]string{"id": req.ID, "state": "connected"})
}

func (d *Deps) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := d.SSH.Disconnect(req.ID); err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	d.persist()
	writeJSON(w, http.StatusOK, map[string]string{"id": req.ID, "state": "disconnected"})
}

func (d *Deps) handleReconnect(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !decode(w, r, &req) {
		return
	}
	s, err := d.SSH.Reconnect(req.ID)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": s.ID, "state": string(s.Info().State)})
}

// --- tools ----------------------------------------------------------------

func (d *Deps) handleTools(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, d.Toggler.List())
}

func (d *Deps) handleToolSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if !decode(w, r, &req) {
		return
	}
	if !d.Toggler.Set(req.Name, req.Enabled) {
		writeErr(w, http.StatusNotFound, "unknown tool "+req.Name)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"name": req.Name, "enabled": req.Enabled})
}

// persist asks the flusher to write state soon. The API mutates the same
// manager the tools do; the manager's own Persist path lives on State, which
// the toggler already calls for tool changes — for session changes we go
// through the store via the debounced flusher wired in main.
func (d *Deps) persist() {
	// The SSH manager doesn't expose a persist hook; State does, but webui
	// deliberately doesn't depend on the MCP State. The daemon's flusher is
	// driven by MCP-side Persist calls; for session connect/disconnect done
	// here we trigger a synchronous save via the store+manager directly.
	if d.Store == nil {
		return
	}
	snap := state.Capture(d.SSH, d.Toggler.DisabledNames())
	_ = d.Store.Save(snap)
}
