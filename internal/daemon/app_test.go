package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/mcptest"
)

// These tests exercise the daemon's HTTP surface — the Host/Origin guard, the
// WebUI API and token auth, exposure gating, and tool toggling — against the
// production wiring (NewApp) served in-process by httptest. Nothing here needs
// a remote host, Docker, or a spawned process, so they run in the default
// unit-test suite. Only the parts that genuinely need a real sshd (shell
// execution, SFTP, reconnect) stay in test/e2e.

// initRequest is the JSON-RPC initialize body a bare HTTP client sends.
var initRequest = func() string {
	b, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "t", "version": "1"},
		},
	})
	if err != nil {
		panic(err)
	}
	return string(b)
}()

// --- fixture ---------------------------------------------------------------

// startApp builds the daemon surface with $HOME pointed at a temp dir holding
// one concrete Host per alias, serves it on a loopback port, and tears it down
// at test cleanup. BoundAddr is the listener's real address so the guard sees
// the same port a browser would.
func startApp(t *testing.T, aliases []string, opts AppOptions) *testApp {
	t.Helper()
	fakeHome(t, aliases...)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	opts.BoundAddr = ln.Addr().String()
	if opts.StatePath == "" {
		opts.StatePath = filepath.Join(t.TempDir(), "state.json")
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	opts.Log = slog.New(slog.NewTextHandler(io.Discard, nil))

	a, err := NewApp(opts)
	if err != nil {
		_ = ln.Close()
		t.Fatalf("NewApp: %v", err)
	}

	// Track the client's server->client stream (GET /mcp) so a notification
	// test can wait for the subscription instead of racing it.
	streamOpen := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/mcp" {
			select {
			case streamOpen <- struct{}{}:
			default:
			}
		}
		a.Handler.ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(handler)
	_ = srv.Listener.Close()
	srv.Listener = ln
	srv.Start()

	ta := &testApp{t: t, App: a, srv: srv, addr: ln.Addr().String(), streamOpen: streamOpen}
	t.Cleanup(ta.stop)
	return ta
}

// fakeHome writes a minimal ~/.ssh/config with one concrete Host per alias and
// points $HOME at it, so the daemon's host list is deterministic and the
// developer's real ssh config is never read.
func fakeHome(t *testing.T, aliases ...string) {
	t.Helper()
	dir := t.TempDir()
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	var cfg strings.Builder
	for _, alias := range aliases {
		fmt.Fprintf(&cfg, "Host %s\n  HostName 10.0.0.1\n  User alice\n", alias)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(cfg.String()), 0o600); err != nil {
		t.Fatalf("write ssh config: %v", err)
	}
	t.Setenv("HOME", dir)
}

// testApp is a running daemon surface. Embedding *App keeps the collaborators
// (Store, Token, …) reachable without re-plumbing them through the fixture.
type testApp struct {
	t *testing.T
	*App
	srv        *httptest.Server
	addr       string
	cookie     string        // session cookie after login; empty means unauthenticated
	streamOpen chan struct{} // fires when a client opens GET /mcp
	once       sync.Once
}

// stop shuts the surface down. Idempotent so restart tests can stop app #1
// explicitly and rely on cleanup for app #2.
func (a *testApp) stop() {
	a.once.Do(func() {
		a.srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		a.StopMCP(ctx)
		a.Close()
	})
}

// login performs the one-time token → cookie exchange.
func (a *testApp) login() {
	a.t.Helper()
	code, cookie := a.exchange(a.Token)
	if code != 200 {
		a.t.Fatalf("token exchange = %d", code)
	}
	a.cookie = cookie
}

// exchange POSTs a token to /api/auth/exchange; returns status + cookie.
func (a *testApp) exchange(token string) (int, string) {
	a.t.Helper()
	body, err := json.Marshal(map[string]string{"token": token})
	if err != nil {
		a.t.Fatalf("marshal token: %v", err)
	}
	req, err := http.NewRequest("POST", "http://"+a.addr+"/api/auth/exchange", strings.NewReader(string(body)))
	if err != nil {
		a.t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == "rsm_auth" {
			cookie = c.String()
		}
	}
	return resp.StatusCode, cookie
}

// api sends one /api request carrying the session cookie, if any.
func (a *testApp) api(method, path string, body any, out any) int {
	a.t.Helper()
	var rdr io.Reader = strings.NewReader("")
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			a.t.Fatalf("marshal body: %v", err)
		}
		rdr = strings.NewReader(string(b))
	}
	req, err := http.NewRequest(method, "http://"+a.addr+"/api"+path, rdr)
	if err != nil {
		a.t.Fatalf("new req: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a.cookie != "" {
		req.Header.Set("Cookie", a.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

// raw fires one request with explicit Host/Origin/content-type control and
// returns the status code — the guard tests need to lie about all three.
func (a *testApp) raw(method, path, host, origin, contentType, body string) int {
	a.t.Helper()
	req, err := http.NewRequest(method, "http://"+a.addr+path, strings.NewReader(body))
	if err != nil {
		a.t.Fatalf("new request: %v", err)
	}
	if host != "" {
		req.Host = host
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// body GETs a page and returns its text, failing on a non-200.
func (a *testApp) body(path string) string {
	a.t.Helper()
	req, err := http.NewRequest("GET", "http://"+a.addr+path, nil)
	if err != nil {
		a.t.Fatalf("new req: %v", err)
	}
	if a.cookie != "" {
		req.Header.Set("Cookie", a.cookie)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		a.t.Fatalf("read body: %v", err)
	}
	if resp.StatusCode != 200 {
		a.t.Fatalf("GET %s = %d", path, resp.StatusCode)
	}
	return string(b)
}

// mcpTools returns the tool names an MCP client currently sees.
func (a *testApp) mcpTools() map[string]bool {
	a.t.Helper()
	mc := mcptest.New(a.t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	names, err := mc.ListTools(ctx)
	if err != nil {
		a.t.Fatalf("tools/list: %v", err)
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// toolCallable reports whether a call to name reaches a registered tool. A
// disabled tool fails the JSON-RPC call; an enabled tool answers it — even
// when the answer is a tool-level error (ssh_file_read with no arguments).
func (a *testApp) toolCallable(name string) bool {
	a.t.Helper()
	mc := mcptest.New(a.t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := mc.CallTool(ctx, name, nil)
	return err == nil
}

type hostsView struct {
	Hosts []struct {
		Alias   string `json:"alias"`
		Exposed bool   `json:"exposed"`
	} `json:"hosts"`
	Error string `json:"error"`
}

func (a *testApp) hosts() hostsView {
	a.t.Helper()
	var view hostsView
	if code := a.api("GET", "/hosts", nil, &view); code != 200 {
		a.t.Fatalf("GET /api/hosts = %d", code)
	}
	if view.Error != "" {
		a.t.Fatalf("hosts error: %s", view.Error)
	}
	return view
}

func (a *testApp) setExposed(alias string, exposed bool) {
	a.t.Helper()
	if code := a.api("POST", "/hosts/set", map[string]any{"alias": alias, "exposed": exposed}, nil); code != 200 {
		a.t.Fatalf("POST /api/hosts/set %s=%v = %d", alias, exposed, code)
	}
}
