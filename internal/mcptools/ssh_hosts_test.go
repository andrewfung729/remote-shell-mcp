package mcptools

import (
	"testing"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func TestWithHostsExposureDefaultsOff(t *testing.T) {
	got := WithHostsExposure(nil, false)
	if len(got) != 1 || got[0] != SSHHostsTool {
		t.Fatalf("none exposed = %v", got)
	}
	got = WithHostsExposure([]string{"ssh_exec", SSHHostsTool}, false)
	if len(got) != 2 || got[0] != "ssh_exec" || got[1] != SSHHostsTool {
		t.Fatalf("explicit off = %v", got)
	}
	got = WithHostsExposure([]string{"ssh_exec", SSHHostsTool}, true)
	if len(got) != 1 || got[0] != "ssh_exec" {
		t.Fatalf("explicit on = %v", got)
	}
}

func TestHostsExposedFrom(t *testing.T) {
	if HostsExposedFrom(nil) != true {
		t.Fatal("empty disabled set is exposed")
	}
	if HostsExposedFrom([]string{SSHHostsTool}) != false {
		t.Fatal("listed tool is hidden")
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
