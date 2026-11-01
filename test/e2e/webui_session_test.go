//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// Session lifecycle through the WebUI API against a real sshd — the UI's
// connect/disconnect/reconnect buttons exercise these routes directly
// (issue #1 stories 15-17).
func TestWebUISessionLifecycle(t *testing.T) {
	mc, sshd, d, teardown := setupSession(t)
	defer teardown()
	_ = mc // the API path doesn't need an MCP client, but setup gives us sshd+daemon

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_ = ctx

	// Connect via the API.
	spec := sshSpec(t, sshd.SSHPort)
	var connResp struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if code := apiJSON(t, d, "POST", "/sessions/connect", map[string]any{"id": "w1", "spec": spec}, &connResp); code != 200 {
		t.Fatalf("POST /api/sessions/connect = %d", code)
	}
	if connResp.ID != "w1" {
		t.Fatalf("connect returned id=%q", connResp.ID)
	}

	// It shows up in GET /api/sessions.
	var sess []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if code := apiJSON(t, d, "GET", "/sessions", nil, &sess); code != 200 {
		t.Fatalf("GET /api/sessions = %d", code)
	}
	found := false
	for _, s := range sess {
		if s.ID == "w1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("w1 not in /api/sessions: %+v", sess)
	}

	// Reconnect via the API.
	if code := apiJSON(t, d, "POST", "/sessions/reconnect", map[string]any{"id": "w1"}, nil); code != 200 {
		t.Fatalf("POST /api/sessions/reconnect = %d", code)
	}

	// Disconnect via the API — w1 leaves the list.
	if code := apiJSON(t, d, "POST", "/sessions/disconnect", map[string]any{"id": "w1"}, nil); code != 200 {
		t.Fatalf("POST /api/sessions/disconnect = %d", code)
	}
	if code := apiJSON(t, d, "GET", "/sessions", nil, &sess); code != 200 {
		t.Fatalf("GET /api/sessions after disconnect = %d", code)
	}
	for _, s := range sess {
		if s.ID == "w1" {
			t.Fatalf("w1 still listed after disconnect")
		}
	}
}
