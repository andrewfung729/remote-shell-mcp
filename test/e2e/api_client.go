//go:build e2e

package e2e

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// apiJSON drives the daemon's /api surface for the tests that need a real
// sshd: exposing a host, listing sessions, revoking exposure.

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
