//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// Session lifecycle visible through the WebUI API against a real sshd — the
// UI's disconnect/reconnect buttons exercise these routes. Sessions are
// created by agents via ssh_connect; the API intentionally has no connect
// route (issue #1 stories 15-17).
func TestWebUISessionLifecycle(t *testing.T) {
	mc, sshd, d, teardown := setupSession(t)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// Connect via MCP — the same way an agent does.
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "w1", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("ssh_connect: %v", err)
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
