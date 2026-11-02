//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// These are the regression tests for the Host/Origin guard, converted from
// the hostile-request PoC in issue #1: a request with a foreign Origin and a
// text/plain body, and a request with a spoofed Host, must NOT reach the
// daemon's routes. No sshd needed — the guard fires before any SSH work.

// httpReq fires one raw HTTP request at the daemon with explicit Host/Origin
// control and returns the status code.
func httpReq(t *testing.T, daemon *Daemon, method, path, host, origin, contentType, body string) int {
	t.Helper()
	url := "http://" + daemon.Addr() + path
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
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
		t.Fatalf("request %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// startGuardDaemon boots a daemon on a free port with no sshd — the guard
// lives at the HTTP edge, before any session work.
func startGuardDaemon(t *testing.T) *Daemon {
	t.Helper()
	daemonBin := BuildDaemon(t)
	d := StartDaemon(t, daemonBin)
	t.Cleanup(d.Stop)
	return d
}

func TestGuardRejectsForeignOrigin(t *testing.T) {
	d := startGuardDaemon(t)
	if code := httpReq(t, d, "POST", "/mcp", "", "https://evil.example", "text/plain", "{}"); code != http.StatusForbidden {
		t.Fatalf("hostile-Origin POST /mcp = %d, want 403", code)
	}
	if code := httpReq(t, d, "POST", "/api/sessions/reconnect", "", "https://evil.example", "text/plain", "{}"); code != http.StatusForbidden {
		t.Fatalf("hostile-Origin POST /api = %d, want 403", code)
	}
	if code := httpReq(t, d, "GET", "/", "", "https://evil.example", "", ""); code != http.StatusForbidden {
		t.Fatalf("hostile-Origin GET / = %d, want 403", code)
	}
}

func TestGuardRejectsSpoofedHost(t *testing.T) {
	d := startGuardDaemon(t)
	for _, path := range []string{"/mcp", "/api/status", "/"} {
		method := "GET"
		if path != "/" {
			method = "POST"
		}
		if code := httpReq(t, d, method, path, "evil.example", "", "application/json", "{}"); code != http.StatusForbidden {
			t.Fatalf("spoofed-Host %s %s = %d, want 403", method, path, code)
		}
	}
	if code := httpReq(t, d, "GET", "/", "127.0.0.1:1", "", "", ""); code != http.StatusForbidden {
		t.Fatalf("wrong-port Host = %d, want 403", code)
	}
	// A port-less loopback Host resolves to the default port, not the
	// daemon's — a rebind to another local process on :80 must not pass.
	if code := httpReq(t, d, "GET", "/", "127.0.0.1", "", "", ""); code != http.StatusForbidden {
		t.Fatalf("port-less Host = %d, want 403", code)
	}
	// A port-less loopback Origin (implicit :80/:443) is a different origin.
	if code := httpReq(t, d, "GET", "/", d.Addr(), "http://127.0.0.1", "", ""); code != http.StatusForbidden {
		t.Fatalf("port-less Origin = %d, want 403", code)
	}
}

func TestGuardAllowsNativeClient(t *testing.T) {
	d := startGuardDaemon(t)
	if code := httpReq(t, d, "GET", "/", "", "", "", ""); code == http.StatusForbidden {
		t.Fatalf("no-Origin GET / = %d, want anything but 403", code)
	}
	init := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	if code := httpReq(t, d, "POST", "/mcp", d.Addr(), "", "application/json", init); code == http.StatusForbidden {
		t.Fatalf("loopback-Host MCP initialize = %d, want not 403", code)
	}
}

func TestGuardRequiresJSONForAPIPost(t *testing.T) {
	d := startGuardDaemon(t)
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		if code := httpReq(t, d, "POST", "/api/tools/set", d.Addr(), "", ct, "x=1"); code != http.StatusUnsupportedMediaType {
			t.Fatalf("POST /api with content-type %q = %d, want 415", ct, code)
		}
	}
	if code := httpReq(t, d, "POST", "/api/tools/set", d.Addr(), "", "application/json", "{}"); code == http.StatusUnsupportedMediaType {
		t.Fatalf("POST /api with application/json = %d, want not 415", code)
	}
}

func TestGuardBoundPortRespected(t *testing.T) {
	d := startGuardDaemon(t)
	if code := httpReq(t, d, "GET", "/", d.Addr(), "", "", ""); code == http.StatusForbidden {
		t.Fatalf("Host with bound port = %d, want not 403", code)
	}
	if code := httpReq(t, d, "GET", "/", d.Addr(), "http://"+d.Addr(), "", ""); code == http.StatusForbidden {
		t.Fatalf("same-origin Origin on bound port = %d, want not 403", code)
	}
}
