package daemon

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/jaenster/remote-shell-mcp/internal/mcptest"
)

// --- page, status, auth ----------------------------------------------------

func TestWebUIStatus(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	a.login()
	var st struct {
		Uptime    string `json:"uptime"`
		Version   string `json:"version"`
		Listen    string `json:"listen"`
		StatePath string `json:"state_path"`
	}
	if code := a.api("GET", "/status", nil, &st); code != 200 {
		t.Fatalf("GET /api/status = %d", code)
	}
	if st.Version != "test" || st.Listen != a.addr || st.StatePath != a.Store.Path() {
		t.Fatalf("status incomplete: %+v", st)
	}
}

func TestWebUIPageServed(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	if code := a.raw("GET", "/", a.addr, "", "", ""); code != 200 {
		t.Fatalf("GET / = %d, want 200", code)
	}
}

func TestWebUIAuthRequired(t *testing.T) {
	a := startApp(t, nil, AppOptions{})

	// No cookie → every /api route is 401.
	for _, p := range []string{"/status", "/sessions", "/hosts", "/tools"} {
		if code := a.api("GET", p, nil, nil); code != http.StatusUnauthorized {
			t.Fatalf("GET /api%s without cookie = %d, want 401", p, code)
		}
	}
	// The page itself is public — it carries no data; its fetches 401 until
	// the operator pastes the token.
	if code := a.raw("GET", "/", a.addr, "", "", ""); code != 200 {
		t.Fatalf("GET / = %d, want 200", code)
	}
	// /mcp stays unauthenticated — MCP clients need no token.
	if code := a.raw("POST", "/mcp", a.addr, "", "application/json", initRequest); code == 401 || code == 403 {
		t.Fatalf("/mcp initialize = %d, want not-4xx-auth", code)
	}
}

func TestWebUITokenExchange(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	if a.Token == "" {
		t.Fatal("no WebUI token minted")
	}
	// Bad token → 403.
	if code, _ := a.exchange("wrong-token"); code != 403 {
		t.Fatalf("bad token exchange = %d, want 403", code)
	}
	// Real token → 200 + cookie; cookie then authorizes /api.
	code, cookie := a.exchange(a.Token)
	if code != 200 {
		t.Fatalf("token exchange = %d", code)
	}
	if cookie == "" {
		t.Fatal("no rsm_auth cookie set")
	}
	a.cookie = cookie
	if code := a.api("GET", "/status", nil, nil); code != 200 {
		t.Fatalf("GET /api/status with cookie = %d", code)
	}
	// Replay: the token is single-use — a second exchange fails even with
	// the same value.
	if code, _ := a.exchange(a.Token); code != 403 {
		t.Fatalf("token replay = %d, want 403", code)
	}
}

// --- hosts / exposure ------------------------------------------------------

func TestWebUIHostsHiddenUntilExposed(t *testing.T) {
	a := startApp(t, []string{"testhost", "otherhost"}, AppOptions{})
	a.login()

	page := a.body("/")
	if !strings.Contains(page, "SSH Hosts") || !strings.Contains(page, "exposes that host") {
		t.Fatalf("page missing per-host switches")
	}
	view := a.hosts()
	if len(view.Hosts) != 2 {
		t.Fatalf("hosts = %+v, want the two fake aliases", view.Hosts)
	}
	for _, h := range view.Hosts {
		if h.Exposed {
			t.Fatalf("host %s exposed by default", h.Alias)
		}
	}
	tools := a.mcpTools()
	if tools["ssh_hosts"] || tools["ssh_shell"] {
		t.Fatalf("gated tools listed before any host was exposed: %v", tools)
	}

	a.setExposed("testhost", true)
	tools = a.mcpTools()
	if !tools["ssh_hosts"] || !tools["ssh_shell"] {
		t.Fatalf("gated tools not listed after exposing testhost: %v", tools)
	}
	for _, h := range a.hosts().Hosts {
		if h.Alias == "testhost" && !h.Exposed {
			t.Fatalf("testhost not marked exposed")
		}
		if h.Alias != "testhost" && h.Exposed {
			t.Fatalf("%s exposed along with testhost", h.Alias)
		}
	}

	a.setExposed("testhost", false)
	tools = a.mcpTools()
	if tools["ssh_hosts"] || tools["ssh_shell"] {
		t.Fatalf("gated tools still listed after the last host was turned off: %v", tools)
	}
}

func TestWebUIHostsExposurePersists(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	a1 := startApp(t, []string{"testhost", "otherhost"}, AppOptions{StatePath: statePath})
	a1.login()
	a1.setExposed("testhost", true)
	a1.stop() // flushes state

	a2 := startApp(t, []string{"testhost", "otherhost"}, AppOptions{StatePath: statePath})
	a2.login()
	if !a2.mcpTools()["ssh_hosts"] {
		t.Fatalf("ssh_hosts hidden after restart; exposed host was not persisted")
	}
	for _, h := range a2.hosts().Hosts {
		if h.Alias == "testhost" && !h.Exposed {
			t.Fatalf("testhost lost its exposure across restart")
		}
		if h.Alias == "otherhost" && h.Exposed {
			t.Fatalf("otherhost became exposed across restart")
		}
	}

	// The state file holds only the exposure list and disabled tools — never
	// session specs or credential material.
	a2.stop()
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if !strings.Contains(string(data), "exposed_hosts") {
		t.Fatalf("state file missing exposure list: %s", data)
	}
	for _, leak := range []string{"ssh_sessions", "IdentityFile", "id_ed25519"} {
		if strings.Contains(string(data), leak) {
			t.Fatalf("state file leaked %s: %s", leak, data)
		}
	}
}

// --- tools -----------------------------------------------------------------

func TestWebUIToolToggleRoundTrip(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	a.login()

	var tools []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if code := a.api("GET", "/tools", nil, &tools); code != 200 {
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
	if !a.toolCallable("ssh_file_read") {
		t.Fatalf("ssh_file_read not callable while enabled")
	}

	if code := a.api("POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": false}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set disable = %d", code)
	}
	if a.mcpTools()["ssh_file_read"] {
		t.Fatalf("ssh_file_read still in MCP tools/list after disable")
	}
	if a.toolCallable("ssh_file_read") {
		t.Fatalf("ssh_file_read still callable after disable")
	}

	if code := a.api("POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": true}, nil); code != 200 {
		t.Fatalf("POST /api/tools/set enable = %d", code)
	}
	if !a.mcpTools()["ssh_file_read"] {
		t.Fatalf("ssh_file_read not back in MCP tools/list after re-enable")
	}
	if !a.toolCallable("ssh_file_read") {
		t.Fatalf("ssh_file_read not callable after re-enable")
	}
}

// TestWebUIToolToggleNotifiesListChanged asserts the daemon emits the
// tools/list_changed notification when a toggle flips, so MCP clients
// refresh their view.
func TestWebUIToolToggleNotifiesListChanged(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	a.login()

	tr, err := transport.NewStreamableHTTP("http://"+a.addr+"/mcp", transport.WithContinuousListening())
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	mc := mcptest.NewWithTransport(t, tr)
	defer mc.Close()

	got := make(chan string, 4)
	mc.OnNotification(func(n mcp.JSONRPCNotification) {
		got <- n.Method
	})
	// The client opens its server->client stream asynchronously. Wait for it
	// rather than racing it, so the assertion is about the very first toggle.
	select {
	case <-a.streamOpen:
	case <-time.After(10 * time.Second):
		t.Fatalf("client never opened its server->client stream")
	}

	if code := a.api("POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": false}, nil); code != 200 {
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

func TestWebUIToolTogglePersistsAcrossRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "state.json")
	a1 := startApp(t, nil, AppOptions{StatePath: statePath})
	a1.login()
	if code := a1.api("POST", "/tools/set", map[string]any{"name": "ssh_file_read", "enabled": false}, nil); code != 200 {
		t.Fatalf("disable: %d", code)
	}
	a1.stop() // flushes state

	a2 := startApp(t, nil, AppOptions{StatePath: statePath})
	a2.login()
	if a2.mcpTools()["ssh_file_read"] {
		t.Fatalf("ssh_file_read re-appeared after restart; disabled state not persisted")
	}
}
