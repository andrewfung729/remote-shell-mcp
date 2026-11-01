package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mark3labs/mcp-go/server"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
	"github.com/jaenster/remote-shell-mcp/internal/dockerx"
	"github.com/jaenster/remote-shell-mcp/internal/mcptools"
	"github.com/jaenster/remote-shell-mcp/internal/sshx"
	"github.com/jaenster/remote-shell-mcp/internal/state"
)

const (
	// Default addr binds the loopback to a kernel-picked free port. The actual
	// bound address is written to daemon.json so clients can find us without
	// hard-coding any port — sidesteps the well-known-port conflict (the
	// previous default, 7800, is JGroups/JBoss).
	defaultAddr = "127.0.0.1:0"
	serverName  = "remote-shell-mcp"
	serverVer   = "0.1.0"
)

func main() {
	addr := flag.String("addr", envOr("REMOTE_SHELL_MCP_ADDR", defaultAddr), "Bind address for the SSE MCP server (host:port). Port 0 means \"pick a free port\".")
	statePath := flag.String("state", envOr("REMOTE_SHELL_MCP_STATE", ""), "Path to state.json (default: $XDG_CONFIG_HOME/remote-shell-mcp/state.json).")
	lockPath := flag.String("lock", envOr("REMOTE_SHELL_MCP_LOCK", ""), "Path to daemon lock file.")
	handlePath := flag.String("handle", envOr("REMOTE_SHELL_MCP_HANDLE", ""), "Path to the handle file. Daemon writes {addr,token,pid} here after binding so clients can find it.")
	logFmt := flag.String("log", envOr("REMOTE_SHELL_MCP_LOG", "text"), "Log format: text or json.")
	outFmt := flag.String("format", envOr("REMOTE_SHELL_MCP_FORMAT", "toon"), "Tool-result output format: toon (default) or json. TOON is ~30-50% smaller for arrays of uniform objects (docker_containers, ssh_file_list, etc.) — see https://github.com/toon-format/spec.")
	requireAuth := flag.Bool("require-auth", envOr("REMOTE_SHELL_MCP_REQUIRE_AUTH", "") == "true", "Require the bearer token even on loopback binds. Non-loopback binds always require it.")
	flag.Parse()

	format, err := mcptools.ParseFormat(*outFmt)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	defLock, defState, defHandle, err := daemon.DefaultPaths()
	if err != nil {
		fmt.Fprintln(os.Stderr, "config dir:", err)
		os.Exit(1)
	}
	if *lockPath == "" {
		*lockPath = defLock
	}
	if *statePath == "" {
		*statePath = defState
	}
	if *handlePath == "" {
		*handlePath = defHandle
	}
	_ = os.MkdirAll(filepath.Dir(*lockPath), 0o700)

	var handler slog.Handler
	if *logFmt == "json" {
		handler = slog.NewJSONHandler(os.Stderr, nil)
	} else {
		handler = slog.NewTextHandler(os.Stderr, nil)
	}
	log := slog.New(handler)

	lock, err := daemon.AcquireLock(*lockPath)
	if err != nil {
		log.Error("acquire lock", "err", err, "path", *lockPath)
		os.Exit(1)
	}
	defer lock.Release()

	token, err := daemon.GenerateToken()
	if err != nil {
		log.Error("generate token", "err", err)
		os.Exit(1)
	}

	// Bind the listener up-front so we know the actual port before we publish
	// the handle. Anything launched after we write daemon.json can trust the
	// addr in there; nothing can race us, because the handle never points at
	// a port we haven't already bound.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err, "addr", *addr)
		os.Exit(1)
	}
	boundAddr := ln.Addr().String()

	if err := daemon.WriteHandle(*handlePath, daemon.Handle{
		Addr:  boundAddr,
		Token: token,
		PID:   os.Getpid(),
	}); err != nil {
		_ = ln.Close()
		log.Error("write handle", "err", err, "path", *handlePath)
		os.Exit(1)
	}
	defer os.Remove(*handlePath)

	store, err := state.NewStore(*statePath)
	if err != nil {
		log.Error("init state store", "err", err)
		os.Exit(1)
	}
	sshMgr := sshx.NewManager()
	dkMgr := dockerx.NewManager()
	st := &mcptools.State{SSH: sshMgr, Docker: dkMgr, Store: store, Log: log, Format: format}

	if snap, err := store.Load(); err != nil {
		log.Warn("load state", "err", err)
	} else if snap != nil {
		state.Restore(snap, sshMgr, dkMgr, log)
	}

	// Start the debounced state flusher before serving traffic so the first
	// tool calls don't fall back to synchronous writes. We do NOT defer Stop()
	// here — Stop calls PersistNow as its last act, and we need that to happen
	// BEFORE CloseAll empties the managers (otherwise the final write
	// captures empty state and clobbers everything that was just saved).
	st.Start()

	mcpServer := mcptools.Build(st, serverName, serverVer)
	streamable := server.NewStreamableHTTPServer(mcpServer,
		server.WithEndpointPath("/mcp"),
		server.WithHeartbeatInterval(15*time.Second),
	)
	// Two transports: /rpc is one-shot JSON-RPC POST; /mcp is the spec
	// Streamable HTTP endpoint remote MCP clients register against.
	mux := http.NewServeMux()
	mux.Handle("/rpc", daemon.RPCHandler(mcpServer))
	mux.Handle("/mcp", streamable)
	// Loopback binds serve local clients and the embedded WebUI — both
	// already hold machine-local trust, so bearer auth is skipped there.
	// A non-loopback bind (LAN/tunnel exposure) keeps the token requirement.
	var root http.Handler = mux
	if *requireAuth || !isLoopbackBind(boundAddr) {
		root = daemon.AuthMiddleware(token, mux)
	} else {
		log.Info("loopback bind: bearer auth disabled", "addr", boundAddr)
	}
	httpSrv := &http.Server{
		Handler: root,
		// Defend against slowloris from a local process holding a connection
		// open without sending headers. SSE handlers explicitly want long-lived
		// connections, so we don't set ReadTimeout / WriteTimeout (which would
		// kill the stream); ReadHeaderTimeout only applies to the request line
		// + headers.
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", boundAddr, "mcp", "/mcp", "rpc", "/rpc",
			"state", store.Path(), "handle", *handlePath, "format", string(format))
		errCh <- httpSrv.Serve(ln)
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Error("server stopped", "err", err)
		}
	case sig := <-sigCh:
		log.Info("signal received", "sig", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = streamable.Shutdown(ctx)
	_ = httpSrv.Shutdown(ctx)

	// Stop the flusher (drains its goroutine and does one final synchronous
	// write of CURRENT state, before we tear sessions down). Order matters:
	// Stop → CloseAll, never the reverse.
	st.Stop()
	sshMgr.CloseAll()
	dkMgr.CloseAll()
	log.Info("shutdown complete")
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// isLoopbackBind reports whether addr's host part is a loopback IP or a name
// that resolves there ("localhost"). Port is ignored.
func isLoopbackBind(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	return ip.IsLoopback()
}
