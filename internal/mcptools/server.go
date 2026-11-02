package mcptools

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
	"github.com/jaenster/remote-shell-mcp/internal/state"
)

type State struct {
	SSH   *sshx.Manager
	Store *state.Store
	Log   *slog.Logger

	// Guard is the read-before-edit record store behind ssh_file_edit: one
	// in-memory hash per (client, host, path), lost on daemon restart. Its
	// zero value is ready to use.
	Guard ReadGuard

	// DisabledTools, when set, supplies the names of the currently-disabled
	// MCP tools so a state flush persists them. main() wires this to the
	// ToolToggler after the server is built.
	DisabledTools func() []string

	exposeMu     sync.Mutex
	exposedHosts []string

	flushStartOnce sync.Once
	flushReqCh     chan struct{} // capacity 1; coalesced "please flush" signal
	flushStopCh    chan struct{}
	flushDoneCh    chan struct{}
}

// Start kicks off the background state flusher. Tool handlers call Persist()
// which is now a debounced async signal; the actual disk write happens here.
// Idempotent; safe to call from main() before serving traffic.
func (s *State) Start() {
	s.flushStartOnce.Do(func() {
		s.flushReqCh = make(chan struct{}, 1)
		s.flushStopCh = make(chan struct{})
		s.flushDoneCh = make(chan struct{})
		go s.flushLoop()
	})
}

// Stop halts the flusher and performs one final synchronous flush so a clean
// shutdown can't leave a pending write on the floor.
func (s *State) Stop() {
	if s.flushStopCh == nil {
		_ = s.PersistNow()
		return
	}
	close(s.flushStopCh)
	<-s.flushDoneCh
	_ = s.PersistNow()
}

// Persist requests an async flush. Multiple calls inside the debounce window
// (200ms) coalesce into a single disk write. Tool handlers call this on
// every state-mutating op; the cost is now bounded regardless of churn.
func (s *State) Persist() error {
	if s.Store == nil {
		return nil
	}
	if s.flushReqCh == nil {
		// Not started — fall back to synchronous so anyone using State as a
		// library outside the daemon still gets persistence semantics.
		return s.PersistNow()
	}
	select {
	case s.flushReqCh <- struct{}{}:
	default:
	}
	return nil
}

// PersistNow blocks until the current state is on disk. Use this from the
// `snapshot` tool and from the daemon shutdown path; otherwise prefer Persist.
func (s *State) PersistNow() error {
	if s.Store == nil {
		return nil
	}
	var disabled []string
	if s.DisabledTools != nil {
		disabled = s.DisabledTools()
	}
	return s.Store.Save(state.Capture(disabled, s.ExposedHosts()))
}

// ExposedHosts returns a copy of the aliases the user has exposed to agents.
func (s *State) ExposedHosts() []string {
	s.exposeMu.Lock()
	defer s.exposeMu.Unlock()
	if len(s.exposedHosts) == 0 {
		return nil
	}
	return append([]string{}, s.exposedHosts...)
}

// SetExposedHosts replaces the exposed-alias set. An empty list clears it.
func (s *State) SetExposedHosts(aliases []string) {
	s.exposeMu.Lock()
	defer s.exposeMu.Unlock()
	if len(aliases) == 0 {
		s.exposedHosts = nil
		return
	}
	s.exposedHosts = append([]string{}, aliases...)
}

func (s *State) flushLoop() {
	defer close(s.flushDoneCh)
	const debounce = 200 * time.Millisecond
	var timer *time.Timer
	var timerC <-chan time.Time
	fire := func() {
		if err := s.PersistNow(); err != nil && s.Log != nil {
			s.Log.Warn("flush state", "err", err)
		}
	}
	for {
		select {
		case <-s.flushStopCh:
			return
		case <-s.flushReqCh:
			if timer == nil {
				timer = time.NewTimer(debounce)
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(debounce)
			}
			timerC = timer.C
		case <-timerC:
			timerC = nil
			timer = nil
			fire()
		}
	}
}

func Build(st *State, reg *ClientRegistry, name, version string) *server.MCPServer {
	srv := server.NewMCPServer(
		name, version,
		server.WithToolCapabilities(true),
		server.WithLogging(),
		server.WithInstructions(serverInstructions),
		server.WithHooks(reg.Hooks()),
	)
	RegisterSSH(srv, st)
	RegisterSSHHosts(srv, st)
	RegisterSFTP(srv, st)
	return srv
}

// serverInstructions is sent to the MCP client in the initialize response.
// Keep it short — it lands in every agent's context.
const serverInstructions = `Remote access over SSH through this server's tools. Do NOT shell out to the local ssh/scp/sftp/rsync CLI — use the ssh_* tools here instead. Hosts must be exposed in the WebUI first; ssh_hosts lists the exposed aliases. ssh_shell runs commands on a host's persistent shell (cd/env survive between calls) and also drives a running command (poll/input/interrupt) or resets it. ssh_file_* / ssh_upload / ssh_download move files on the same hosts.`

// resultJSON serializes a result for an MCP text-content reply. All tool
// results are JSON — the alternative output format was removed (issue #1).
func (s *State) resultJSON(v any) (*mcp.CallToolResult, error) {
	return jsonResult(v)
}

func jsonResult(v any) (*mcp.CallToolResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return mcp.NewToolResultErrorFromErr("encode result", err), nil
	}
	return mcp.NewToolResultText(string(data)), nil
}

func resultErr(err error) (*mcp.CallToolResult, error) {
	return mcp.NewToolResultError(err.Error()), nil
}

// caller returns a stable identifier for the calling MCP client — its
// clientInfo.name from initialize, falling back to the transport session id
// for clients that don't send one. Shells are owned by this identity: each
// client sees and drives only its own shells on a shared host session.
func caller(ctx context.Context) string {
	s := server.ClientSessionFromContext(ctx)
	if s == nil {
		return ""
	}
	if sci, ok := s.(server.SessionWithClientInfo); ok {
		if name := sci.GetClientInfo().Name; name != "" {
			return name
		}
	}
	return s.SessionID()
}

func bind[T any](req mcp.CallToolRequest, out *T) error {
	return req.BindArguments(out)
}

// IsExposed reports whether the user has exposed this host alias to agents.
func (s *State) IsExposed(alias string) bool {
	s.exposeMu.Lock()
	defer s.exposeMu.Unlock()
	for _, a := range s.exposedHosts {
		if a == alias {
			return true
		}
	}
	return false
}
