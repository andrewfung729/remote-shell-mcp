package mcptools

import (
	"testing"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func TestWithHostsExposureDefaultsOff(t *testing.T) {
	got := WithHostsExposure(nil, false)
	if len(got) != len(exposureGatedTools) {
		t.Fatalf("none exposed = %v", got)
	}
	for _, g := range exposureGatedTools {
		found := false
		for _, n := range got {
			if n == g {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s not disabled when nothing exposed: %v", g, got)
		}
	}
	got = WithHostsExposure([]string{"ssh_file_read", SSHHostsTool, SSHShellTool}, false)
	if len(got) != 1+len(exposureGatedTools) || got[0] != "ssh_file_read" {
		t.Fatalf("explicit off = %v", got)
	}
	got = WithHostsExposure([]string{"ssh_file_read", SSHHostsTool, SSHShellTool}, true)
	if len(got) != 1 || got[0] != "ssh_file_read" {
		t.Fatalf("explicit on = %v", got)
	}
}

func TestExposedOnly(t *testing.T) {
	hosts := []sshx.ConfiguredHost{{Alias: "a"}, {Alias: "b"}, {Alias: "c"}}
	got := exposedOnly(hosts, []string{"c", "a", "missing"})
	if len(got) != 2 || got[0].Alias != "a" || got[1].Alias != "c" {
		t.Fatalf("got %+v", got)
	}
	if len(exposedOnly(hosts, nil)) != 0 {
		t.Fatal("nil set should yield no hosts")
	}
}

func TestValidateShellArgs(t *testing.T) {
	str := func(s string) *string { return &s }
	tru := func() *bool { b := true; return &b }
	cases := []struct {
		name    string
		args    shellArgs
		want    shellAction
		wantErr bool
	}{
		{"command", shellArgs{Host: "h", Command: str("ls")}, actCommand, false},
		{"empty command counts", shellArgs{Host: "h", Command: str("")}, actCommand, false},
		{"input", shellArgs{Host: "h", Input: str("y")}, actInput, false},
		{"interrupt", shellArgs{Host: "h", Interrupt: tru()}, actInterrupt, false},
		{"poll", shellArgs{Host: "h", Poll: tru()}, actPoll, false},
		{"reset", shellArgs{Host: "h", Reset: tru()}, actReset, false},
		{"no action", shellArgs{Host: "h"}, actNone, true},
		{"no host", shellArgs{Command: str("ls")}, actNone, true},
		{"two actions", shellArgs{Host: "h", Command: str("ls"), Poll: tru()}, actNone, true},
		{"false flag is not an action", shellArgs{Host: "h", Command: str("ls"), Poll: boolp(false)}, actCommand, false},
	}
	for _, c := range cases {
		got, err := validateShellArgs(c.args)
		if c.wantErr {
			if err == nil {
				t.Fatalf("%s: expected error", c.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != c.want {
			t.Fatalf("%s: action %d, want %d", c.name, got, c.want)
		}
	}
}

func boolp(b bool) *bool { return &b }
