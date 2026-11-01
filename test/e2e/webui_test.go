//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

// WebUI API + tool-toggling e2e. These exercise the real daemon over HTTP at
// the /api surface — no sshd needed for the toggling/status paths.

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

func TestWebUIStatus(t *testing.T) {
	d := startGuardDaemon(t)
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
	d := startGuardDaemon(t)
	if code := httpReq(t, d, "GET", "/", d.Addr(), "", "", ""); code != 200 {
		t.Fatalf("GET / = %d, want 200", code)
	}
}

func TestWebUIToolToggleRoundTrip(t *testing.T) {
	d := startGuardDaemon(t)
	var tools []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if code := apiJSON(t, d, "GET", "/tools", nil, &tools); code != 200 {
		t.Fatalf("GET /api/tools = %d", code)
	}
	found := false
	for _, tt := range tools {
		if tt.Name == "ssh_exec" && tt.Enabled {
			found = true
		}
	}
	if !found {
		t.Fatalf("ssh_exec not listed enabled: %+v", tools)
	}
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_exec", "enabled": false}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set disable = %d", code)
	}
	if toolListed(t, d, "ssh_exec") {
		t.Fatalf("ssh_exec still in MCP tools/list after disable")
	}
	// A disabled tool must also FAIL when called — not just vanish from the list.
	if toolCallable(t, d, "ssh_exec") {
		t.Fatalf("ssh_exec still callable after disable")
	}
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_exec", "enabled": true}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set enable = %d", code)
	}
	if !toolListed(t, d, "ssh_exec") {
		t.Fatalf("ssh_exec not back in MCP tools/list after re-enable")
	}
}

// TestWebUIToolToggleNotifiesListChanged asserts the daemon emits the
// tools/list_changed notification when a toggle flips, so MCP clients
// refresh their view (issue #1 story 27).
func TestWebUIToolToggleNotifiesListChanged(t *testing.T) {
	d := startGuardDaemon(t)
	// The notification only reaches a client that holds the streamable GET
	// stream open, so this test needs a continuous-listening transport.
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
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_exec", "enabled": false}, nil); code != 200 {
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
// "tool not found" JSON-RPC error a disabled/unknown tool produces). We only
// need reachability, so the error string distinguishes them.
func toolCallable(t *testing.T, d *Daemon, name string) bool {
	t.Helper()
	mc := NewMCPClientForDaemon(t, d)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Call with a deliberately-bad arg; a live tool returns a *tool* error
	// (IsError), a missing tool returns the transport-level "not found".
	res, err := mc.c.CallTool(ctx, mcp.CallToolRequest{
		Params: mcp.CallToolParams{Name: name},
	})
	if err != nil {
		return false // JSON-RPC error: tool not found
	}
	_ = res
	return true // got a CallToolResult back (even an IsError one)
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
	if code := apiJSON(t, d1, "POST", "/tools/set", map[string]any{"name": "ssh_exec", "enabled": false}, nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	d1.Stop()

	d2 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	defer d2.Stop()
	if toolListed(t, d2, "ssh_exec") {
		t.Fatalf("ssh_exec re-appeared after restart; disabled state not persisted")
	}
}

// --- startup bind safety (issue #1, no sshd needed) -------------------------

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
	d1 := startGuardDaemon(t) // holds its port
	code, _ := runDaemonExpectExit(t, daemonBin,
		"-addr", d1.Addr(), "-state", t.TempDir()+"/s.json",
		"-lock", t.TempDir()+"/l", "-handle", t.TempDir()+"/h")
	if code == 0 {
		t.Fatalf("port conflict did not exit non-zero")
	}
}
