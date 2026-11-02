package sshx

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfiguredHostsConcreteAliasesOnly(t *testing.T) {
	dir := t.TempDir()
	inc := filepath.Join(dir, "included")
	if err := os.WriteFile(inc, []byte(""+
		"Host frominc\n"+
		"  User bob\n"+
		"  HostName 10.0.0.2\n"+
		"  ProxyJump bastion\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(dir, "config")
	body := "" +
		"Host *\n" +
		"  ServerAliveInterval 30\n" +
		"\n" +
		"Host box other\n" +
		"  User alice\n" +
		"  HostName 10.0.0.1\n" +
		"  Port 2222\n" +
		"  IdentityFile ~/.ssh/secret\n" +
		"\n" +
		"Host *.wild ?\n" +
		"  User wild\n" +
		"\n" +
		"Match host box\n" +
		"  ForwardAgent yes\n" +
		"\n" +
		"Include " + inc + "\n"
	if err := os.WriteFile(root, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ConfiguredHosts(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("hosts = %+v, want box, other, frominc", got)
	}
	if got[0].Alias != "box" || got[0].User != "alice" || got[0].Hostname != "10.0.0.1" || got[0].Port != "2222" {
		t.Fatalf("box = %+v", got[0])
	}
	if got[1].Alias != "other" || got[1].Hostname != "10.0.0.1" {
		t.Fatalf("other = %+v", got[1])
	}
	if got[2].Alias != "frominc" || got[2].User != "bob" || got[2].Hostname != "10.0.0.2" || got[2].Port != "22" || got[2].ProxyJump != "bastion" {
		t.Fatalf("frominc = %+v", got[2])
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "IdentityFile") {
		t.Fatalf("key path leaked into host list: %s", raw)
	}
}

func TestConfiguredHostsMissingFile(t *testing.T) {
	got, err := ConfiguredHosts(filepath.Join(t.TempDir(), "nope"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}
