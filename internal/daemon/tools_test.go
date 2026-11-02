package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/mcptest"
)

func TestToolListSurface(t *testing.T) {
	a := startApp(t, []string{"testhost"}, AppOptions{})
	a.login()
	a.setExposed("testhost", true)

	tools := a.mcpTools()
	for _, gone := range []string{"ssh_connect", "ssh_disconnect", "ssh_sessions", "ssh_reconnect", "ssh_exec", "ssh_shell_open", "ssh_shell_write", "ssh_shell_read", "ssh_shell_resize", "ssh_shell_close", "ssh_shell_list"} {
		if tools[gone] {
			t.Fatalf("removed tool %s still listed", gone)
		}
	}
	for _, want := range []string{"ssh_hosts", "ssh_shell", "ssh_file_read", "ssh_upload"} {
		if !tools[want] {
			t.Fatalf("expected tool %s not listed: %v", want, tools)
		}
	}

	a.setExposed("testhost", false)
	tools = a.mcpTools()
	if tools["ssh_hosts"] || tools["ssh_shell"] {
		t.Fatalf("gated tools visible with no exposed host: %v", tools)
	}
	a.setExposed("testhost", true)
	tools = a.mcpTools()
	if !tools["ssh_hosts"] || !tools["ssh_shell"] {
		t.Fatalf("gated tools not restored after re-expose: %v", tools)
	}
}

// TestUnexposedHostRejected covers the exposure boundary as an agent sees it:
// the shell and file tools refuse a host the user hasn't exposed, and answer
// identically whether or not the alias exists in ssh config. The refusal is
// decided before any dial, so this needs no sshd.
func TestUnexposedHostRejected(t *testing.T) {
	a := startApp(t, []string{"testhost", "otherhost"}, AppOptions{})
	a.login()
	// ssh_shell only exists while a host is exposed, so open the gate with
	// one host and probe the other aliases behind it.
	a.setExposed("testhost", true)
	mc := mcptest.New(t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// "otherhost" is in ssh config but not exposed; "ghosthost" is nowhere.
	// Both must fail the same way, so an agent cannot probe the config.
	for _, alias := range []string{"otherhost", "ghosthost"} {
		if _, err := mc.CallText(ctx, "ssh_shell", map[string]any{"host": alias, "command": "id"}); err == nil || !strings.Contains(err.Error(), "host not exposed") {
			t.Fatalf("ssh_shell on %s = %v, want host not exposed", alias, err)
		}
		if _, err := mc.CallText(ctx, "ssh_file_list", map[string]any{"host": alias, "path": "/"}); err == nil || !strings.Contains(err.Error(), "host not exposed") {
			t.Fatalf("ssh_file_list on %s = %v, want host not exposed", alias, err)
		}
	}

	// Revoking an exposed host refuses it again. Keep a second host exposed,
	// otherwise the gated tools themselves disappear and there is nothing
	// left to call.
	a.setExposed("otherhost", true)
	a.setExposed("testhost", false)
	if _, err := mc.CallText(ctx, "ssh_shell", map[string]any{"host": "testhost", "command": "id"}); err == nil || !strings.Contains(err.Error(), "host not exposed") {
		t.Fatalf("revoked host = %v, want host not exposed", err)
	}
}
