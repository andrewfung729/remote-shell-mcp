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
	Clients   *mcptools.ClientRegistry
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
	mux.HandleFunc("GET /api/clients", d.handleClients)
	mux.HandleFunc("POST /api/sessions/disconnect", d.handleDisconnect)
	mux.HandleFunc("POST /api/sessions/reconnect", d.handleReconnect)
	mux.HandleFunc("GET /api/hosts", d.handleHosts)
	mux.HandleFunc("POST /api/hosts/set", d.handleHostSet)
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
	ID    string `json:"id"`
	Age   string `json:"age"`
	Owner string `json:"owner,omitempty"` // MCP client name (clientInfo) or session id
}

type sessionView struct {
	ID            string      `json:"id"`
	State         string      `json:"state"`
	User          string      `json:"user"`
	Addresses     []string    `json:"addresses"`
	ActiveAddress string      `json:"active_address,omitempty"`
	Persistent    bool        `json:"persistent"`
	AutoReconnect bool        `json:"auto_reconnect"`
	LastError     string      `json:"last_error,omitempty"`
	Shells        []shellView `json:"shells"`
}

func (d *Deps) handleSessions(w http.ResponseWriter, r *http.Request) {
	sessions := d.SSH.Sessions()
	out := make([]sessionView, 0, len(sessions))
	for _, s := range sessions {
		info := s.Info()
		shs := s.AllShells()
		shells := make([]shellView, 0, len(shs))
		for _, sh := range shs {
			shells = append(shells, shellView{ID: sh.ID, Age: time.Since(sh.OpenedAt).Round(time.Second).String(), Owner: sh.Owner})
		}
		out = append(out, sessionView{
			ID:            info.ID,
			State:         info.State,
			User:          info.User,
			Addresses:     info.Addresses,
			ActiveAddress: info.ActiveAddress,
			Persistent:    info.Persistent,
			AutoReconnect: info.AutoReconnect,
			LastError:     info.LastError,
			Shells:        shells,
		})
	}
	writeJSON(w, http.StatusOK, out)
}

// handleClients returns the live MCP clients for the WebUI's clients list.
func (d *Deps) handleClients(w http.ResponseWriter, r *http.Request) {
	if d.Clients == nil {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	writeJSON(w, http.StatusOK, d.Clients.List())
}

// --- session mutations -----------------------------------------------------

// idReq is the {id} body the single-session mutations share.
type idReq struct {
	ID string `json:"id"`
}

func (d *Deps) handleDisconnect(w http.ResponseWriter, r *http.Request) {
	var req idReq
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
	var req idReq
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

// --- hosts ----------------------------------------------------------------

type hostView struct {
	sshx.ConfiguredHost
	Exposed bool `json:"exposed"`
}

type hostsResp struct {
	Hosts []hostView `json:"hosts"`
	Error string     `json:"error,omitempty"`
}

func (d *Deps) handleHosts(w http.ResponseWriter, r *http.Request) {
	resp := hostsResp{Hosts: []hostView{}}
	hosts, err := sshx.ConfiguredHosts("")
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	on := map[string]bool{}
	if d.Toggler != nil {
		for _, a := range d.Toggler.ExposedHosts() {
			on[a] = true
		}
	}
	resp.Hosts = make([]hostView, 0, len(hosts))
	for _, h := range hosts {
		resp.Hosts = append(resp.Hosts, hostView{ConfiguredHost: h, Exposed: on[h.Alias]})
	}
	writeJSON(w, http.StatusOK, resp)
}

func (d *Deps) handleHostSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Alias   string `json:"alias"`
		Exposed bool   `json:"exposed"`
	}
	if !decode(w, r, &req) {
		return
	}
	if d.Toggler == nil {
		writeErr(w, http.StatusInternalServerError, "tool toggler unavailable")
		return
	}
	hosts, err := sshx.ConfiguredHosts("")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	found := false
	for _, h := range hosts {
		if h.Alias == req.Alias {
			found = true
			break
		}
	}
	if !found {
		writeErr(w, http.StatusNotFound, "unknown host "+req.Alias)
		return
	}
	d.Toggler.SetExposedHost(req.Alias, req.Exposed)
	writeJSON(w, http.StatusOK, map[string]any{"alias": req.Alias, "exposed": req.Exposed})
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
	// ssh_hosts is visible only while at least one host is exposed. Turning
	// the tool off clears those switches. Turning it on with none selected
	// does not expose every host.
	if req.Name == mcptools.SSHHostsTool {
		if !req.Enabled {
			d.Toggler.ClearExposedHosts()
		}
		if req.Enabled && len(d.Toggler.ExposedHosts()) == 0 {
			writeJSON(w, http.StatusOK, map[string]any{"name": req.Name, "enabled": false})
			return
		}
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
	disabled := d.Toggler.DisabledNames()
	snap := state.Capture(d.SSH, disabled, d.Toggler.ExposedHosts())
	_ = d.Store.Save(snap)
}
