//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// WebUI API + tool-toggling e2e. These exercise the real daemon over HTTP
// at the /api surface — token auth first, then the cookie.

// apiJSON sends one /api request carrying the session cookie the daemon
// issued (d.authCookie, set by exchangeToken). Tests that never exchanged
// get 401 — which is exactly what the auth tests assert on.
func apiJSON(t *testing.T, d *Daemon, method, path string, body map[string]any, out any) int {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req, err := http.NewRequest(method, "http://"+d.Addr()+"/api"+path, rdr)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if d.authCookie != "" {
		req.Header.Set("Cookie", d.authCookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// startAuthedDaemon boots a daemon and completes the token exchange so the
// returned Daemon's /api calls are authenticated.
func startAuthedDaemon(t *testing.T) *Daemon {
	t.Helper()
	daemonBin := BuildDaemon(t)
	d := StartDaemon(t, daemonBin)
	t.Cleanup(d.Stop)
	exchangeToken(t, d)
	return d
}

func TestWebUIStatus(t *testing.T) {
	d := startAuthedDaemon(t)
	var st struct {
		Uptime    string `json:"uptime"`
		Version   string `json:"version"`
		Listen    string `json:"listen"`
		StatePath string `json:"state_path"`
	}
	if code := apiJSON(t, d, "GET", "/status", nil, &st); code != 200 {
		t.Fatalf("GET /api/status = %d", code)
	}
	if st.Version == "" || st.Listen != d.Addr() || st.StatePath == "" {
		t.Fatalf("status incomplete: %+v", st)
	}
}

func TestWebUIPageServed(t *testing.T) {
	d := startAuthedDaemon(t)
	if code := httpReq(t, d, "GET", "/", d.Addr(), "", "", ""); code != 200 {
		t.Fatalf("GET / = %d, want 200", code)
	}
}

// --- WebUI auth (issue #14) ---------------------------------------------------

func TestWebUIAuthRequired(t *testing.T) {
	daemonBin := BuildDaemon(t)
	d := StartDaemon(t, daemonBin)
	t.Cleanup(d.Stop)

	// No cookie → every /api route is 401.
	for _, p := range []string{"/status", "/sessions", "/hosts", "/tools"} {
		if code := apiJSON(t, d, "GET", p, nil, nil); code != 401 {
			t.Fatalf("GET /api%s without cookie = %d, want 401", p, code)
		}
	}
	// The page itself is public — it carries no data; its fetches 401 until
	// the operator pastes the token.
	if code := httpReq(t, d, "GET", "/", d.Addr(), "", "", ""); code != 200 {
		t.Fatalf("GET / = %d, want 200", code)
	}
	// /mcp stays unauthenticated — MCP clients need no token.
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	if code := httpReq(t, d, "POST", "/mcp", d.Addr(), "", "application/json", init); code == 401 || code == 403 {
		t.Fatalf("/mcp initialize = %d, want not-4xx-auth", code)
	}
}

func TestWebUITokenExchange(t *testing.T) {
	daemonBin := BuildDaemon(t)
	d := StartDaemon(t, daemonBin)
	t.Cleanup(d.Stop)

	token := d.Token()
	if token == "" {
		t.Fatal("daemon printed no token")
	}
	// Bad token → 403.
	if code, _ := apiExchange(t, d, "wrong-token"); code != 403 {
		t.Fatalf("bad token exchange = %d, want 403", code)
	}
	// Real token → 200 + cookie; cookie then authorizes /api.
	code, cookie := apiExchange(t, d, token)
	if code != 200 {
		t.Fatalf("token exchange = %d", code)
	}
	if cookie == "" {
		t.Fatal("no rsm_auth cookie set")
	}
	d.authCookie = cookie
	if code := apiJSON(t, d, "GET", "/status", nil, nil); code != 200 {
		t.Fatalf("GET /api/status with cookie = %d", code)
	}
	// Replay: the token is single-use — a second exchange fails even with
	// the same value.
	if code, _ := apiExchange(t, d, token); code != 403 {
		t.Fatalf("token replay = %d, want 403", code)
	}
}

// --- hosts / exposure ---------------------------------------------------------

type hostsView struct {
	Hosts []struct {
		Alias   string `json:"alias"`
		Exposed bool   `json:"exposed"`
	} `json:"hosts"`
	Error string `json:"error"`
}

func getHosts(t *testing.T, d *Daemon) hostsView {
	t.Helper()
	var view hostsView
	if code := apiJSON(t, d, "GET", "/hosts", nil, &view); code != 200 {
		t.Fatalf("GET /api/hosts = %d", code)
	}
	if view.Error != "" {
		t.Fatalf("hosts error: %s", view.Error)
	}
	return view
}

func TestWebUIHostsHiddenUntilExposed(t *testing.T) {
	d := startAuthedDaemon(t)
	body := httpBody(t, d, "GET", "/")
	if !strings.Contains(body, "SSH Hosts") || !strings.Contains(body, "exposes that host") {
		t.Fatalf("page missing per-host switches")
	}
	view := getHosts(t, d)
	for _, h := range view.Hosts {
		if h.Exposed {
			t.Fatalf("host %s exposed by default", h.Alias)
		}
	}
	if toolListed(t, d, "ssh_hosts") || toolListed(t, d, "ssh_shell") {
		t.Fatalf("gated tools listed before any host was exposed")
	}
	if len(view.Hosts) == 0 {
		t.Skip("no concrete hosts in ssh config")
	}
	alias := view.Hosts[0].Alias
	if code := apiJSON(t, d, "POST", "/hosts/set", map[string]any{"alias": alias, "exposed": true}, nil); code != 200 {
		t.Fatalf("expose %s = %d", alias, code)
	}
	if !toolListed(t, d, "ssh_hosts") || !toolListed(t, d, "ssh_shell") {
		t.Fatalf("gated tools not listed after exposing %s", alias)
	}
	view = getHosts(t, d)
	for _, h := range view.Hosts {
		if h.Alias == alias && !h.Exposed {
			t.Fatalf("%s not marked exposed", alias)
		}
		if h.Alias != alias && h.Exposed {
			t.Fatalf("%s exposed along with %s", h.Alias, alias)
		}
	}
	if code := apiJSON(t, d, "POST", "/hosts/set", map[string]any{"alias": alias, "exposed": false}, nil); code != 200 {
		t.Fatalf("hide %s = %d", alias, code)
	}
	if toolListed(t, d, "ssh_hosts") || toolListed(t, d, "ssh_shell") {
		t.Fatalf("gated tools still listed after the last host was turned off")
	}
}

func TestWebUIHostsExposurePersists(t *testing.T) {
	daemonBin := BuildDaemon(t)
	dir := t.TempDir()
	statePath := dir + "/state.json"
	lockPath := dir + "/daemon.lock"
	d1 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	exchangeToken(t, d1)
	view := getHosts(t, d1)
	if len(view.Hosts) < 2 {
		d1.Stop()
		t.Skip("need at least two concrete hosts to check per-host persistence")
	}
	alias := view.Hosts[0].Alias
	other := view.Hosts[1].Alias
	if code := apiJSON(t, d1, "POST", "/hosts/set", map[string]any{"alias": alias, "exposed": true}, nil); code != 200 {
		t.Fatalf("expose: %d", code)
	}
	d1.Stop()

	d2 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	defer d2.Stop()
	exchangeToken(t, d2)
	if !toolListed(t, d2, "ssh_hosts") {
		t.Fatalf("ssh_hosts hidden after restart; exposed host was not persisted")
	}
	view = getHosts(t, d2)
	for _, h := range view.Hosts {
		if h.Alias == alias && !h.Exposed {
			t.Fatalf("%s lost its exposure across restart", alias)
		}
		if h.Alias == other && h.Exposed {
			t.Fatalf("%s became exposed across restart", other)
		}
	}
}

func httpBody(t *testing.T, d *Daemon, method, path string) string {
	t.Helper()
	req, err := http.NewRequest(method, "http://"+d.Addr()+path, nil)
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	if d.authCookie != "" {
		req.Header.Set("Cookie", d.authCookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("%s %s = %d", method, path, resp.StatusCode)
	}
	return string(b)
}

// --- tools --------------------------------------------------------------------

func TestWebUIToolToggleRoundTrip(t *testing.T) {
	d := startAuthedDaemon(t)
	var tools []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if code := apiJSON(t, d, "GET", "/tools", nil, &tools); code != 200 {
		t.Fatalf("GET /api/tools = %d", code)
	}
	found := false
	for _, tt := range tools {
		if tt.Name == "ssh_file_read" && tt.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatalf("ssh_file_read not listed enabled: %+v", tools)
	}
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": false}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set disable = %d", code)
	}
	if toolListed(t, d, "ssh_file_read") {
		t.Fatalf("ssh_file_read still in MCP tools/list after disable")
	}
	if toolCallable(t, d, "ssh_file_read") {
		t.Fatalf("ssh_file_read still callable after disable")
	}
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": true}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set enable = %d", code)
	}
	if !toolListed(t, d, "ssh_file_read") {
		t.Fatalf("ssh_file_read not back in MCP tools/list after re-enable")
	}
}

// TestWebUIToolToggleNotifiesListChanged asserts the daemon emits the
// tools/list_changed notification when a toggle flips, so MCP clients
// refresh their view.
func TestWebUIToolToggleNotifiesListChanged(t *testing.T) {
	d := startAuthedDaemon(t)
	tr, err := transport.NewStreamableHTTP("http://"+d.Addr()+"/mcp", transport.WithContinuousListening())
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	mc := &MCPClient{t: t, c: mcpclient.NewClient(tr)}
	if err := mc.c.Start(context.Background()); err != nil {
		t.Fatalf("client start: %v", err)
	}
	defer mc.Close()
	mc.initialize()

	got := make(chan string, 4)
	mc.c.OnNotification(func(n mcp.JSONRPCNotification) {
		got <- n.Method
	})
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": false}, nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	select {
	case m := <-got:
		if m != "notifications/tools/list_changed" {
			t.Fatalf("notification method = %q, want tools/list_changed", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no tools/list_changed notification on disable")
	}
}

// toolCallable reports whether a call to name returns a tool result (vs the
// "tool not found" JSON-RPC error a disabled/unknown tool produces).
func toolCallable(t *testing.T, d *Daemon, name string) bool {
	t.Helper()
	mc := NewMCPClientForDaemon(t, d)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := mc.c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name},
	})
	if err != nil {
		return false
	}
	_ = res
	return true
}

func toolListed(t *testing.T, d *Daemon, name string) bool {
	t.Helper()
	mc := NewMCPClientForDaemon(t, d)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := mc.c.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tt := range res.Tools {
		if tt.Name == name {
			return true
		}
	}
	return false
}

func TestWebUIToolTogglePersistsAcrossRestart(t *testing.T) {
	daemonBin := BuildDaemon(t)
	dir := t.TempDir()
	statePath := dir + "/state.json"
	lockPath := dir + "/daemon.lock"
	d1 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	exchangeToken(t, d1)
	if code := apiJSON(t, d1, "POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": false}, nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	d1.Stop()

	d2 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	defer d2.Stop()
	exchangeToken(t, d2)
	if toolListed(t, d2, "ssh_file_read") {
		t.Fatalf("ssh_file_read re-appeared after restart; disabled state not persisted")
	}
}

// --- startup bind safety --------------------------------------------------------

func runDaemonExpectExit(t *testing.T, daemonBin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(daemonBin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatalf("daemon run: %v", err)
	return -1, ""
}

func TestNonLoopbackAddrRejected(t *testing.T) {
	daemonBin := BuildDaemon(t)
	code, out := runDaemonExpectExit(t, daemonBin,
		"-addr", "0.0.0.0:47499", "-state", t.TempDir()+"/s.json",
		"-lock", t.TempDir()+"/l", "-handle", t.TempDir()+"/h")
	if code == 0 {
		t.Fatalf("non-loopback addr accepted; out=%s", out)
	}
	if !strings.Contains(out, "loopback") {
		t.Fatalf("expected loopback rejection message, got %q", out)
	}
}

func TestPortConflictExitsNonZero(t *testing.T) {
	daemonBin := BuildDaemon(t)
	d1 := startAuthedDaemon(t) // holds its port
	code, _ := runDaemonExpectExit(t, daemonBin,
		"-addr", d1.Addr(), "-state", t.TempDir()+"/s.json",
		"-lock", t.TempDir()+"/l", "-handle", t.TempDir()+"/h")
	if code == 0 {
		t.Fatalf("port conflict did not exit non-zero")
	}
}
