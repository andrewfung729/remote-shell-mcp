//go:build e2e

package e2e

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// Process-level startup and auth tests. These need the real binary — exit
// codes, the stderr message, and the WebUI token the daemon prints to its own
// stdout — but no sshd, so they run without Docker.
//
// Everything below the process boundary (guard, /api routes, exposure gating,
// tool toggles) is covered by the in-process unit tests in internal/daemon.

// TestDaemonPrintsWebUIToken covers the one thing only the real process can
// show: the daemon mints a token, prints it on stdout — nowhere else — and the
// printed value authorizes /api exactly once.
func TestDaemonPrintsWebUIToken(t *testing.T) {
	t.Parallel()
	daemonBin := BuildDaemon(t)
	d := StartDaemon(t, daemonBin)
	t.Cleanup(d.Stop)

	token := d.Token()
	if token == "" {
		t.Fatal("daemon printed no WebUI token")
	}
	code, cookie := apiExchange(t, d, token)
	if code != 200 {
		t.Fatalf("token exchange = %d", code)
	}
	if cookie == "" {
		t.Fatal("no rsm_auth cookie set")
	}
	// The token is single-use: a replay fails.
	if code, _ := apiExchange(t, d, token); code != 403 {
		t.Fatalf("token replay = %d, want 403", code)
	}
}

func runDaemonExpectExit(t *testing.T, daemonBin string, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(daemonBin, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return 0, out.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), out.String()
	}
	t.Fatalf("daemon run: %v", err)
	return -1, ""
}

func TestNonLoopbackAddrRejected(t *testing.T) {
	t.Parallel()
	daemonBin := BuildDaemon(t)
	code, out := runDaemonExpectExit(t, daemonBin,
		"-addr", "0.0.0.0:47499", "-state", t.TempDir()+"/s.json",
		"-lock", t.TempDir()+"/l", "-handle", t.TempDir()+"/h")
	if code == 0 {
		t.Fatalf("non-loopback addr accepted; out=%s", out)
	}
	if !strings.Contains(out, "loopback") {
		t.Fatalf("expected loopback rejection message, got %q", out)
	}
}

func TestPortConflictExitsNonZero(t *testing.T) {
	t.Parallel()
	daemonBin := BuildDaemon(t)
	d1 := StartDaemon(t, daemonBin) // holds its port
	t.Cleanup(d1.Stop)
	exchangeToken(t, d1)

	code, _ := runDaemonExpectExit(t, daemonBin,
		"-addr", d1.Addr(), "-state", t.TempDir()+"/s.json",
		"-lock", t.TempDir()+"/l", "-handle", t.TempDir()+"/h")
	if code == 0 {
		t.Fatalf("port conflict did not exit non-zero")
	}
}
