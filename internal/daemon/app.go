package daemon

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/mcptools"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
	"github.com/jaenster/remote-shell-mcp/internal/state"
	"github.com/jaenster/remote-shell-mcp/internal/webui"
)

// defaultServerName is what the MCP server calls itself in the initialize
// response when the caller doesn't override AppOptions.Name.
const defaultServerName = "remote-shell-mcp"

// AppOptions is everything the HTTP surface needs. Process-level concerns —
// flags, listener, lock file, handle file, signals — stay in main; this is
// only the wiring below the listener.
type AppOptions struct {
	// BoundAddr is the real listener address, e.g. "127.0.0.1:47180". The
	// Host/Origin guard enforces it and /api/status reports it.
	BoundAddr string
	// StatePath is state.json. Empty means the default under
	// $XDG_CONFIG_HOME/remote-shell-mcp/.
	StatePath string
	// Name and Version are the MCP server identity. Empty Name means
	// defaultServerName.
	Name    string
	Version string
	Log     *slog.Logger
}

// App is one daemon's fully wired HTTP surface: the MCP endpoint at /mcp plus
// the WebUI at /api and /, all behind the Host/Origin guard. It owns the
// manager, store, flusher and tool toggler those handlers act on, so there is
// a single wiring path for both the daemon process and its tests.
type App struct {
	// Handler is the guard-wrapped mux: mount the HTTP server on it.
	Handler http.Handler
	// Store is the state store the surface persists into.
	Store *state.Store
	// Token is the one-time WebUI token. Print it to the terminal; it is the
	// only credential the daemon mints and it dies on first exchange.
	Token string

	ssh        *sshx.Manager
	state      *mcptools.State
	toggler    *mcptools.ToolToggler
	streamable *server.StreamableHTTPServer
}

// NewApp builds the surface: load persisted state, apply the exposure gate,
// register the tools, mint the WebUI token, and wrap the mux in the guard. A
// state-file load error is logged and treated as empty state — a corrupt file
// must not brick the daemon.
func NewApp(opts AppOptions) (*App, error) {
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Name == "" {
		opts.Name = defaultServerName
	}

	store, err := state.NewStore(opts.StatePath)
	if err != nil {
		return nil, err
	}
	sshMgr := sshx.NewManager()
	st := &mcptools.State{SSH: sshMgr, Store: store, Log: opts.Log}

	var snap *state.Snapshot
	if s, err := store.Load(); err != nil {
		opts.Log.Warn("load state", "err", err)
	} else {
		snap = s
	}

	// Start the debounced flusher before serving traffic so the first tool
	// calls don't fall back to synchronous writes. Close stops it before
	// tearing sessions down — see the ordering note there.
	st.Start()

	clients := mcptools.NewClientRegistry()
	mcpServer := mcptools.Build(st, clients, opts.Name, opts.Version)

	// Tool toggling: the WebUI flips MCP tools on/off without a restart.
	// Apply the persisted disabled set, then feed future flushes the live set.
	toggler := mcptools.NewToolToggler(mcpServer, st)
	var disabled []string
	if snap != nil {
		disabled = snap.DisabledTools
	}
	configured, cfgErr := sshx.ConfiguredHosts("")
	if cfgErr != nil {
		opts.Log.Warn("read ssh config", "err", cfgErr)
	}
	aliases := make([]string, 0, len(configured))
	for _, h := range configured {
		aliases = append(aliases, h.Alias)
	}
	var exposed []string
	if cfgErr != nil && snap != nil {
		exposed = append([]string{}, snap.ExposedHosts...)
	} else {
		exposed = state.EffectiveExposedHosts(snap, aliases)
	}
	st.SetExposedHosts(exposed)
	// ssh_hosts and ssh_shell stay hidden until the user exposes a host.
	toggler.ApplyDisabled(mcptools.WithHostsExposure(disabled, len(exposed) > 0))
	st.DisabledTools = toggler.DisabledNames

	token := webui.GenerateToken()

	streamable := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath("/mcp"),
		server.WithHeartbeatInterval(15*time.Second),
	)
	// Single transport: /mcp, the spec Streamable HTTP endpoint remote MCP
	// clients register against. Loopback-only binds make this safe to serve
	// unauthenticated.
	mux := http.NewServeMux()
	mux.Handle("/mcp", streamable)
	// The WebUI serves /api/* and the embedded page at /. Its handlers call
	// the manager/store/toggler directly — not MCP tools — so no tool toggle
	// can lock the page out.
	ui := webui.New(&webui.Deps{
		SSH: sshMgr, Store: store, Toggler: toggler, Clients: clients,
		Version: opts.Version, BoundAddr: opts.BoundAddr, Log: opts.Log,
		Token: token,
	})
	mux.Handle("/api/", ui)
	mux.Handle("/", ui)

	return &App{
		// Wrap the whole mux so a hostile web page can't drive the daemon:
		// Host must be loopback on the bound port (DNS-rebinding), any Origin
		// must be same-origin, and /api POSTs must carry a JSON content type.
		Handler:    Guard(opts.BoundAddr, "/api", mux),
		Store:      store,
		Token:      token,
		ssh:        sshMgr,
		state:      st,
		toggler:    toggler,
		streamable: streamable,
	}, nil
}

// StopMCP ends the MCP transport. Call it before draining the HTTP server: its
// server->client streams are long-lived requests, and a plain
// http.Server.Shutdown would wait on them until it times out.
func (a *App) StopMCP(ctx context.Context) {
	_ = a.streamable.Shutdown(ctx)
}

// Close stops the state flusher and then closes every session. The order is
// load-bearing: the flusher's last act is a synchronous write of CURRENT
// state, so closing sessions first would persist a state their teardown just
// emptied. Call it after the HTTP server has stopped.
func (a *App) Close() {
	a.state.Stop()
	a.ssh.CloseAll()
}
