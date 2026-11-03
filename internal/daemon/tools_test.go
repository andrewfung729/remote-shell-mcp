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
	for _, gone := range []string{
		// Session surface removed in issue #14.
		"ssh_connect", "ssh_disconnect", "ssh_sessions", "ssh_reconnect", "ssh_exec",
		"ssh_shell_open", "ssh_shell_write", "ssh_shell_read", "ssh_shell_resize",
		"ssh_shell_close", "ssh_shell_list",
		// Shell-equivalent SFTP mutators removed in issue #24 — the shell is
		// the place for chmod/mkdir/rm/mv.
		"ssh_file_chmod", "ssh_file_mkdir", "ssh_file_delete", "ssh_file_rename",
	} {
		if tools[gone] {
			t.Fatalf("removed tool %s still listed", gone)
		}
	}
	for _, want := range []string{
		"ssh_hosts", "ssh_shell", "ssh_file_list", "ssh_file_stat",
		"ssh_file_read", "ssh_file_write", "ssh_file_edit",
		"ssh_upload", "ssh_download",
	} {
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

	mc := mcptest.New(t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schemas, err := mc.OutputSchemaTypes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for name, typ := range schemas {
		if typ != "" {
			t.Fatalf("tool %s declares an output schema %q", name, typ)
		}
	}
	got, err := mc.CallText(ctx, "ssh_hosts", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "testhost" {
		t.Fatalf("ssh_hosts = %q, want one alias", got)
	}
	// A tool error is the sentence itself, not a JSON object.
	_, err = mc.CallText(ctx, "ssh_shell", map[string]any{"host": "testhost"})
	if err == nil || strings.Contains(err.Error(), "{") || !strings.Contains(err.Error(), "exactly one action") {
		t.Fatalf("tool error = %v, want plain text", err)
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

// TestExposureBeforeArguments pins the ordering every tool follows: exposure
// first, then arguments, then the dial. An unexposed host gets the exposure
// refusal even when the arguments are also wrong, so an agent cannot use a
// validation error to learn that an alias exists.
func TestExposureBeforeArguments(t *testing.T) {
	a := startApp(t, []string{"testhost", "otherhost"}, AppOptions{})
	a.login()
	a.setExposed("testhost", true)
	mc := mcptest.New(t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "no action", args: map[string]any{"host": "otherhost"}},
		{name: "two actions", args: map[string]any{"host": "otherhost", "command": "id", "poll": true}},
		{name: "a bad timeout", args: map[string]any{"host": "otherhost", "timeout_ms": float64(-5)}},
	} {
		if _, err := mc.CallText(ctx, "ssh_shell", tc.args); err == nil || !strings.Contains(err.Error(), "host not exposed") {
			t.Fatalf("ssh_shell with %s = %v, want host not exposed", tc.name, err)
		}
	}

	fileCalls := []struct {
		name string
		args map[string]any
	}{
		{name: "read with byte params", args: map[string]any{"host": "otherhost", "path": "/etc/passwd", "offset": float64(0)}},
		{name: "read with a bad line number", args: map[string]any{"host": "otherhost", "path": "/etc/passwd", "start_line": float64(0)}},
		{name: "edit with no record", args: map[string]any{"host": "otherhost", "path": "/etc/passwd", "old_text": "a", "new_text": "b"}},
		{name: "write with no data", args: map[string]any{"host": "otherhost", "path": "/tmp/x"}},
		{name: "no path at all", args: map[string]any{"host": "otherhost"}},
	}
	for _, tool := range []string{"ssh_file_list", "ssh_file_stat", "ssh_file_read", "ssh_file_write", "ssh_file_edit", "ssh_upload", "ssh_download"} {
		for _, tc := range fileCalls {
			if _, err := mc.CallText(ctx, tool, tc.args); err == nil || !strings.Contains(err.Error(), "host not exposed") {
				t.Fatalf("%s with %s = %v, want host not exposed", tool, tc.name, err)
			}
		}
	}
}

// TestFileArgumentsCheckedBeforeDial covers the argument checks an exposed
// host gets before any connection is opened. The fixture's alias points at an
// address nothing listens on, so a dial would surface as a connection error —
// seeing the argument error proves no dial happened.
func TestFileArgumentsCheckedBeforeDial(t *testing.T) {
	a := startApp(t, []string{"testhost"}, AppOptions{})
	a.login()
	a.setExposed("testhost", true)
	mc := mcptest.New(t, a.addr)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	cases := []struct {
		name string
		tool string
		args map[string]any
		want string
	}{
		{
			name: "offset is rejected by name",
			tool: "ssh_file_read",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "offset": float64(0)},
			want: "offset",
		},
		{
			name: "length is rejected even with start_line",
			tool: "ssh_file_read",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "length": float64(10), "start_line": float64(2)},
			want: "length",
		},
		{
			name: "start_line below one",
			tool: "ssh_file_read",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "start_line": float64(0)},
			want: "start_line",
		},
		{
			name: "max_lines below zero",
			tool: "ssh_file_read",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "max_lines": float64(-1)},
			want: "max_lines",
		},
		{
			name: "fractional start_line",
			tool: "ssh_file_read",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "start_line": 1.5},
			want: "start_line",
		},
		{
			name: "start_line as a string",
			tool: "ssh_file_read",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "start_line": "3"},
			want: "start_line",
		},
		{
			name: "edit without a read",
			tool: "ssh_file_edit",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "old_text": "a", "new_text": "b"},
			want: "ssh_file_read",
		},
		{
			name: "edit with empty old_text",
			tool: "ssh_file_edit",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "old_text": "", "new_text": "b"},
			want: "old_text",
		},
		{
			name: "edit that is a no-op",
			tool: "ssh_file_edit",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "old_text": "a", "new_text": "a"},
			want: "identical",
		},
		{
			name: "edit without new_text",
			tool: "ssh_file_edit",
			args: map[string]any{"host": "testhost", "path": "/tmp/x", "old_text": "a"},
			want: "new_text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := mc.CallText(ctx, tc.tool, tc.args)
			if err == nil {
				t.Fatalf("%s %v = success, want an argument error", tc.tool, tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s %v = %v, want an error mentioning %q", tc.tool, tc.args, err, tc.want)
			}
		})
	}
}
