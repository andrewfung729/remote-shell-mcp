package daemon

import (
	"net/http"
	"testing"
)

// Regression tests for the Host/Origin guard, carried over from the
// hostile-request PoC in issue #1: a request with a foreign Origin and a
// text/plain body, and a request with a spoofed Host, must NOT reach the
// daemon's routes.

func TestGuardRejectsForeignOrigin(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	if code := a.raw("POST", "/mcp", "", "https://evil.example", "text/plain", "{}"); code != http.StatusForbidden {
		t.Fatalf("hostile-Origin POST /mcp = %d, want 403", code)
	}
	if code := a.raw("POST", "/api/sessions/reconnect", "", "https://evil.example", "text/plain", "{}"); code != http.StatusForbidden {
		t.Fatalf("hostile-Origin POST /api = %d, want 403", code)
	}
	if code := a.raw("GET", "/", "", "https://evil.example", "", ""); code != http.StatusForbidden {
		t.Fatalf("hostile-Origin GET / = %d, want 403", code)
	}
}

func TestGuardRejectsSpoofedHost(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	for _, path := range []string{"/mcp", "/api/status", "/"} {
		method := "GET"
		if path != "/" {
			method = "POST"
		}
		if code := a.raw(method, path, "evil.example", "", "application/json", "{}"); code != http.StatusForbidden {
			t.Fatalf("spoofed-Host %s %s = %d, want 403", method, path, code)
		}
	}
	if code := a.raw("GET", "/", "127.0.0.1:1", "", "", ""); code != http.StatusForbidden {
		t.Fatalf("wrong-port Host = %d, want 403", code)
	}
	// A port-less loopback Host resolves to the default port, not the
	// daemon's — a rebind to another local process on :80 must not pass.
	if code := a.raw("GET", "/", "127.0.0.1", "", "", ""); code != http.StatusForbidden {
		t.Fatalf("port-less Host = %d, want 403", code)
	}
	// A port-less loopback Origin (implicit :80/:443) is a different origin.
	if code := a.raw("GET", "/", a.addr, "http://127.0.0.1", "", ""); code != http.StatusForbidden {
		t.Fatalf("port-less Origin = %d, want 403", code)
	}
}

func TestGuardAllowsNativeClient(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	if code := a.raw("GET", "/", "", "", "", ""); code == http.StatusForbidden {
		t.Fatalf("no-Origin GET / = %d, want anything but 403", code)
	}
	if code := a.raw("POST", "/mcp", a.addr, "", "application/json", initRequest); code == http.StatusForbidden {
		t.Fatalf("loopback-Host MCP initialize = %d, want not 403", code)
	}
}

func TestGuardRequiresJSONForAPIPost(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", ""} {
		if code := a.raw("POST", "/api/tools/set", a.addr, "", ct, "x=1"); code != http.StatusUnsupportedMediaType {
			t.Fatalf("POST /api with content-type %q = %d, want 415", ct, code)
		}
	}
	if code := a.raw("POST", "/api/tools/set", a.addr, "", "application/json", "{}"); code == http.StatusUnsupportedMediaType {
		t.Fatalf("POST /api with application/json = %d, want not 415", code)
	}
}

func TestGuardBoundPortRespected(t *testing.T) {
	a := startApp(t, nil, AppOptions{})
	if code := a.raw("GET", "/", a.addr, "", "", ""); code == http.StatusForbidden {
		t.Fatalf("Host with bound port = %d, want not 403", code)
	}
	if code := a.raw("GET", "/", a.addr, "http://"+a.addr, "", ""); code == http.StatusForbidden {
		t.Fatalf("same-origin Origin on bound port = %d, want not 403", code)
	}
}
