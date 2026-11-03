//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"
)

// Session lifecycle visible through the WebUI API against a real sshd —
// the UI's disconnect/reconnect buttons exercise these routes. Sessions are
// created lazily by agents via ssh_shell; the API intentionally has no
// connect route (issue #14).
func TestWebUISessionLifecycle(t *testing.T) {
	t.Parallel()
	mc, _, d, teardown := setupDaemon(t)
	defer teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// A session is opened transparently by the first ssh_shell call.
	if _, err := shellCall(t, mc, ctx, map[string]any{"host": "testhost", "command": "echo hi"}); err != nil {
		t.Fatalf("ssh_shell: %v", err)
	}

	var sess []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if code := apiJSON(t, d, "GET", "/sessions", nil, &sess); code != 200 {
		t.Fatalf("GET /api/sessions = %d", code)
	}
	found := false
	for _, s := range sess {
		if s.ID == "testhost" {
			found = true
		}
	}
	if !found {
		t.Fatalf("testhost not in /api/sessions: %+v", sess)
	}

	// Reconnect via the API.
	if code := apiJSON(t, d, "POST", "/sessions/reconnect", map[string]any{"id": "testhost"}, nil); code != 200 {
		t.Fatalf("POST /api/sessions/reconnect = %d", code)
	}

	// Disconnect via the API — testhost leaves the list.
	if code := apiJSON(t, d, "POST", "/sessions/disconnect", map[string]any{"id": "testhost"}, nil); code != 200 {
		t.Fatalf("POST /api/sessions/disconnect = %d", code)
	}
	if code := apiJSON(t, d, "GET", "/sessions", nil, &sess); code != 200 {
		t.Fatalf("GET /api/sessions after disconnect = %d", code)
	}
	for _, s := range sess {
		if s.ID == "testhost" {
			t.Fatalf("testhost still listed after disconnect")
		}
	}
}
