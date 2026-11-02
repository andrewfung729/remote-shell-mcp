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

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
)

const (
	// Default addr binds loopback on a fixed high port so clients can point at
	// a stable, bookmarkable URL. 47180 is unassigned (no IANA service) and
	// obscure enough to dodge common dev-port collisions; if it IS taken the
	// daemon refuses to start rather than silently picking another port — a
	// client registered against the default must never find a different
	// daemon. Pass -addr/REMOTE_SHELL_MCP_ADDR to override (tests use :0 or
	// explicit addrs).
	defaultAddr = "127.0.0.1:47180"
)

// version is the daemon's reported MCP server version. The release pipeline
// injects the real tag via -ldflags "-X main.version=vX.Y.Z"; this fallback
// is what a plain go build reports.
var version = "dev"

func main() {
	addr := flag.String("addr", envOr("REMOTE_SHELL_MCP_ADDR", defaultAddr), "Bind address for the MCP server (host:port). Loopback only — non-loopback binds are rejected. Port 0 means \"pick a free port\" (tests).")
	statePath := flag.String("state", envOr("REMOTE_SHELL_MCP_STATE", ""), "Path to state.json (default: $XDG_CONFIG_HOME/remote-shell-mcp/state.json).")
	lockPath := flag.String("lock", envOr("REMOTE_SHELL_MCP_LOCK", ""), "Path to daemon lock file.")
	handlePath := flag.String("handle", envOr("REMOTE_SHELL_MCP_HANDLE", ""), "Path to the handle file. Daemon writes {addr,pid} here after binding so clients can find it.")
	logFmt := flag.String("log", envOr("REMOTE_SHELL_MCP_LOG", "text"), "Log format: text or json.")
	flag.Parse()

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

	// Loopback-only daemon: refuse any bind that would expose the MCP
	// surface (unauthenticated) beyond this machine. Validated on the flag
	// value BEFORE binding so a typo fails fast with a clear message.
	if !isLoopbackBind(*addr) {
		fmt.Fprintf(os.Stderr, "remote-shell-mcpd: -addr %q rejected: only loopback addresses (127.0.0.0/8, ::1, localhost) are allowed\n", *addr)
		os.Exit(2)
	}

	lock, err := daemon.AcquireLock(*lockPath)
	if err != nil {
		log.Error("acquire lock", "err", err, "path", *lockPath)
		os.Exit(1)
	}
	defer lock.Release()

	// Bind the listener up-front so we know the actual port before we publish
	// the handle. Anything launched after we write daemon.json can trust the
	// addr in there; nothing can race us, because the handle never points at
	// a port we haven't already bound. On the fixed default port a conflict
	// fails here with EADDRINUSE — deliberate: we never fall back to a random
	// port, since registered clients expect the daemon at the default.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Error("listen", "err", err, "addr", *addr)
		fmt.Fprintf(os.Stderr, "remote-shell-mcpd: cannot bind %s: %v\n", *addr, err)
		os.Exit(1)
	}
	boundAddr := ln.Addr().String()

	if err := daemon.WriteHandle(*handlePath, daemon.Handle{
		Addr: boundAddr,
		PID:  os.Getpid(),
	}); err != nil {
		_ = ln.Close()
		log.Error("write handle", "err", err, "path", *handlePath)
		os.Exit(1)
	}
	defer os.Remove(*handlePath)

	// Everything below the listener — state store, SSH manager, MCP server,
	// tool toggler, WebUI — is wired by daemon.NewApp, which the unit tests
	// build directly. The WebUI token is minted there too: one random value
	// per start, printed once below and exchanged once for the session
	// cookie. Anyone who can read this stdout can open the WebUI — keep the
	// daemon's log private (see README's launchd warning).
	app, err := daemon.NewApp(daemon.AppOptions{
		BoundAddr: boundAddr,
		StatePath: *statePath,
		Version:   version,
		Log:       log,
	})
	if err != nil {
		_ = ln.Close()
		log.Error("init daemon", "err", err)
		os.Exit(1)
	}

	// Defend against slowloris from a local process holding a connection open
	// without sending headers. The streamable transport's GET stream is
	// long-lived, so we don't set ReadTimeout / WriteTimeout (which would
	// kill it); ReadHeaderTimeout only applies to the request line + headers.
	httpSrv := &http.Server{
		Handler:           app.Handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", boundAddr, "mcp", "/mcp",
			"state", app.Store.Path(), "handle", *handlePath)
		errCh <- httpSrv.Serve(ln)
	}()
	// Printed once, on stdout — the operator sees it in the terminal where
	// they started the daemon. It is NOT in the log line above and NOT in
	// the handle file; a local agent must not be able to scrape it.
	fmt.Printf("\nWebUI: http://%s/\n  one-time token: %s\n\n", boundAddr, app.Token)

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
	// Stop the MCP transport first so its long-lived streams don't hold the
	// HTTP drain open; then the flusher, then sessions (see App.Close).
	app.StopMCP(ctx)
	_ = httpSrv.Shutdown(ctx)
	app.Close()
	log.Info("shutdown complete")
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

// isLoopbackBind reports whether addr's host part is a loopback IP or a name
// that resolves there ("localhost"). Port is ignored. An empty host — e.g.
// ":8080", Go's all-interfaces shorthand — is NOT loopback and is rejected.
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
