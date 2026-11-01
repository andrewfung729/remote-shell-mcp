//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

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
	if code := apiJSON(t, d, "POST", "/tools/set", map[string]any{"name": "ssh_exec", "enabled": true}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set enable = %d", code)
	}
	if !toolListed(t, d, "ssh_exec") {
		t.Fatalf("ssh_exec not back in MCP tools/list after re-enable")
	}
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
