//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/mcptest"
)

func TestMain_BinariesAndImage(t *testing.T) {
	t.Parallel()
	_ = BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
}

// shellResp is the ssh_shell JSON result.
type shellResp struct {
	Output         string `json:"output"`
	Status         string `json:"status"`
	ExitCode       *int   `json:"exit_code"`
	CWD            string `json:"cwd"`
	Truncated      bool   `json:"truncated"`
	ShellRestarted bool   `json:"shell_restarted"`
	ShellDegraded  bool   `json:"shell_degraded"`
}

// setupDaemon boots a daemon over a real sshd container with the "testhost"
// alias configured but NOT yet exposed — exposure is per-test via the WebUI
// API so the exposure boundary is exercised end to end.
func setupDaemon(t *testing.T) (*mcptest.Client, *SSHDContainer, *Daemon, func()) {
	t.Helper()
	return setupDaemonImage(t, true)
}

// setupDaemonImage is setupDaemon against either fixture: withBash=false uses
// the bash-less image, i.e. what a minimal host looks like.
func setupDaemonImage(t *testing.T, withBash bool) (*mcptest.Client, *SSHDContainer, *Daemon, func()) {
	t.Helper()
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	var sshd *SSHDContainer
	if withBash {
		sshd = StartSSHDContainer(t)
	} else {
		sshd = StartSSHDContainerNoBash(t)
	}
	home := testHomeForContainer(t, sshd, hostBlock(t, "testhost", sshd.SSHPort))
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{Env: []string{"HOME=" + home}})
	exchangeToken(t, daemon)
	exposeHost(t, daemon, "testhost")
	mc := mcptest.New(t, daemon.Addr())
	teardown := func() {
		mc.Close()
		daemon.Stop()
		sshd.Stop()
	}
	return mc, sshd, daemon, teardown
}

// exchangeToken performs the one-time token → cookie exchange; the cookie
// is stashed on the Daemon so apiJSON sends it on every /api call.
func exchangeToken(t *testing.T, d *Daemon) {
	t.Helper()
	token := d.Token()
	if token == "" {
		t.Fatal("daemon did not print a WebUI token")
	}
	code, cookie := apiExchange(t, d, token)
	if code != 200 {
		t.Fatalf("token exchange = %d", code)
	}
	d.authCookie = cookie
}

// apiExchange POSTs the token to /api/auth/exchange; returns status + cookie.
func apiExchange(t *testing.T, d *Daemon, token string) (int, string) {
	t.Helper()
	body := fmt.Sprintf(`{"token":%q}`, token)
	req, err := http.NewRequest("POST", "http://"+d.Addr()+"/api/auth/exchange", strings.NewReader(body))
	if err != nil {
		t.Fatalf("new req: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	defer resp.Body.Close()
	var cookie string
	for _, c := range resp.Cookies() {
		if c.Name == "rsm_auth" {
			cookie = c.String()
		}
	}
	return resp.StatusCode, cookie
}

func exposeHost(t *testing.T, d *Daemon, alias string) {
	t.Helper()
	if code := apiJSON(t, d, "POST", "/hosts/set", map[string]any{"alias": alias, "exposed": true}, nil); code != 200 {
		t.Fatalf("expose %s = %d", alias, code)
	}
}

func unexposeHost(t *testing.T, d *Daemon, alias string) {
	t.Helper()
	if code := apiJSON(t, d, "POST", "/hosts/set", map[string]any{"alias": alias, "exposed": false}, nil); code != 200 {
		t.Fatalf("unexpose %s = %d", alias, code)
	}
}

// run is the one-call convenience: ssh_shell host+command, must finish.
func run(t *testing.T, mc *mcptest.Client, host, cmd string) shellResp {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var r shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{"host": host, "command": cmd}, &r); err != nil {
		t.Fatalf("ssh_shell %q: %v", cmd, err)
	}
	return r
}

// --- core ssh_shell behavior -------------------------------------------------

func TestShellCommand(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()

	r := run(t, mc, "testhost", "echo hello && pwd && exit 7")
	if r.Status != "done" {
		t.Fatalf("status=%q", r.Status)
	}
	if !strings.Contains(r.Output, "hello") {
		t.Fatalf("output missing: %q", r.Output)
	}
	if r.ExitCode == nil || *r.ExitCode != 7 {
		t.Fatalf("exit_code=%v", r.ExitCode)
	}
	if r.CWD == "" {
		t.Fatal("cwd missing")
	}
	if strings.ContainsAny(r.Output, "") || strings.Contains(r.Output, "__RSM_") {
		t.Fatalf("unclean output: %q", r.Output)
	}
}

func TestShellPersistsState(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()

	run(t, mc, "testhost", "MARK=abc123; cd /tmp")
	r := run(t, mc, "testhost", "echo $MARK $PWD")
	if !strings.Contains(r.Output, "abc123 /tmp") {
		t.Fatalf("state lost: %q", r.Output)
	}
}

func TestShellNamedParallel(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var r shellResp
		_ = mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "shell": "build", "command": "sleep 3; echo build-done", "timeout_ms": 15000,
		}, &r)
	}()
	time.Sleep(500 * time.Millisecond)
	r := run(t, mc, "testhost", "echo default-shell-works")
	<-done
	if !strings.Contains(r.Output, "default-shell-works") {
		t.Fatalf("default shell blocked: %q", r.Output)
	}
}

func TestShellTimeoutThenPoll(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var r shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "command": "sleep 2; echo late-done", "timeout_ms": 300,
	}, &r); err != nil {
		t.Fatalf("command: %v", err)
	}
	if r.Status != "running" {
		t.Fatalf("status=%q want running", r.Status)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var p shellResp
		if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "poll": true, "timeout_ms": 5000,
		}, &p); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if p.Status == "done" {
			if !strings.Contains(p.Output, "late-done") {
				t.Fatalf("poll lost output: %q", p.Output)
			}
			return
		}
	}
	t.Fatal("poll never saw command finish")
}

func TestShellBusyError(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var r shellResp
		_ = mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "command": "sleep 5; echo x", "timeout_ms": 10000,
		}, &r)
	}()
	time.Sleep(500 * time.Millisecond)
	_, err := mc.CallText(ctx, "ssh_shell", map[string]any{"host": "testhost", "command": "echo nope"})
	if err == nil || !strings.Contains(err.Error(), "shell busy") {
		t.Fatalf("expected shell busy error, got %v", err)
	}
	_, _ = mc.Call(ctx, "ssh_shell", map[string]any{"host": "testhost", "interrupt": true})
	<-done
}

func TestShellInterrupt(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var r shellResp
		_ = mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "command": "sleep 60; echo never", "timeout_ms": 20000,
		}, &r)
	}()
	time.Sleep(500 * time.Millisecond)
	if _, err := mc.Call(ctx, "ssh_shell", map[string]any{"host": "testhost", "interrupt": true}); err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	var p shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "poll": true, "timeout_ms": 8000,
	}, &p); err != nil {
		t.Fatalf("poll: %v", err)
	}
	<-done
	if p.Status != "done" {
		t.Fatalf("status=%q — interrupt did not kill the command", p.Status)
	}
	if strings.Contains(p.Output, "never") {
		t.Fatalf("interrupted command ran to completion: %q", p.Output)
	}
	if p.ExitCode != nil && *p.ExitCode == 0 {
		t.Fatalf("expected non-zero exit after interrupt, got %d", *p.ExitCode)
	}
}

func TestShellInputInteractive(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The command's own call is still waiting for its marker, so it may be the
	// one that collects the result. Both callers are legitimate; the assertion
	// is that the fed line reached the command, not which call returned it.
	var cmdRes shellResp
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "command": "read -p ANSWER: ans; echo got-$ans", "timeout_ms": 15000,
		}, &cmdRes)
	}()
	time.Sleep(800 * time.Millisecond)
	if _, err := mc.Call(ctx, "ssh_shell", map[string]any{"host": "testhost", "input": "hello-input" + string(byte(10))}); err != nil {
		t.Fatalf("input: %v", err)
	}
	var p shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "poll": true, "timeout_ms": 8000,
	}, &p); err != nil {
		t.Fatalf("poll: %v", err)
	}
	<-done
	if out := cmdRes.Output + p.Output; !strings.Contains(out, "got-hello-input") {
		t.Fatalf("interactive input did not reach the command: %q", out)
	}
}

func TestShellReset(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()

	run(t, mc, "testhost", "cd /tmp; MARKER=set")
	if _, err := mc.Call(context.Background(), "ssh_shell", map[string]any{"host": "testhost", "reset": true}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	r := run(t, mc, "testhost", "echo $MARKER $PWD")
	if !strings.Contains(r.Output, " $") && !strings.Contains(r.Output, " /root") {
		// After reset the var is gone and cwd is the login dir.
		t.Logf("post-reset output: %q", r.Output)
	}
	if strings.Contains(r.Output, "set /tmp") {
		t.Fatalf("reset kept state: %q", r.Output)
	}
}

// --- exposure boundary --------------------------------------------------------

func TestRevokeClosesSessionAndShells(t *testing.T) {
	t.Parallel()
	mc, _, d, teardown := setupDaemon(t)
	defer teardown()

	run(t, mc, "testhost", "cd /tmp")

	unexposeHost(t, d, "testhost")
	var sess []struct {
		ID string `json:"id"`
	}
	if code := apiJSON(t, d, "GET", "/sessions", nil, &sess); code != 200 {
		t.Fatalf("GET /api/sessions = %d", code)
	}
	for _, s := range sess {
		if s.ID == "testhost" {
			t.Fatalf("session still listed after unexpose")
		}
	}
	exposeHost(t, d, "testhost")
	r := run(t, mc, "testhost", "pwd")
	if r.Status != "done" {
		t.Fatalf("post-revoke shell dead: %+v", r)
	}
	if r.CWD == "/tmp" {
		t.Fatalf("cwd survived revocation: %q", r.CWD)
	}
	if !r.ShellRestarted {
		t.Fatalf("expected shell_restarted after revoke, got %+v", r)
	}
}

// --- transport/respawn -------------------------------------------------------

func TestShellReconnectRespawns(t *testing.T) {
	t.Parallel()
	mc, sshd, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	run(t, mc, "testhost", "cd /tmp; echo before-kill")
	if _, err := exec.Command("docker", "exec", sshd.ID, "sh", "-c", "pkill -KILL sshd").CombinedOutput(); err != nil {
		t.Fatalf("kill sshd: %v", err)
	}
	deadline := time.Now().Add(45 * time.Second)
	var r shellResp
	for time.Now().Before(deadline) {
		var res shellResp
		err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "command": "echo after-kill", "timeout_ms": 15000,
		}, &res)
		if err == nil && res.Status == "done" && strings.Contains(res.Output, "after-kill") {
			r = res
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !strings.Contains(r.Output, "after-kill") {
		t.Fatalf("session never recovered after sshd kill: %+v", r)
	}
	if !r.ShellRestarted {
		t.Fatalf("expected shell_restarted after reconnect: %+v", r)
	}
	if r.CWD == "/tmp" {
		t.Fatalf("cwd survived reconnect: %q", r.CWD)
	}
}

// --- rc fixtures: noisy / broken ----------------------------------------------

func TestShellDegradedOnBrokenRC(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/root/.bashrc",
		"data": "sleep 60\n",
	}); err != nil {
		t.Fatalf("write broken rc: %v", err)
	}
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{"host": "testhost", "reset": true}, &shellResp{}); err != nil {
		t.Fatalf("reset after broken rc: %v", err)
	}
	var r2 shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "command": "echo still-works",
	}, &r2); err != nil {
		t.Fatalf("command on degraded shell: %v", err)
	}
	if !r2.ShellDegraded {
		t.Fatalf("expected shell_degraded with broken rc: %+v", r2)
	}
	if !strings.Contains(r2.Output, "still-works") {
		t.Fatalf("degraded shell didn't run: %q", r2.Output)
	}
	_, _ = mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/root/.bashrc", "data": "",
	})
}

func TestShellCleanUnderNoisyRC(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/root/.bashrc",
		"data": "echo BANNER-NOISE\nexport PS1='CUSTOM> '\nPROMPT_COMMAND='echo HOOK-NOISE'\n",
	}); err != nil {
		t.Fatalf("write noisy rc: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_shell", map[string]any{"host": "testhost", "reset": true}); err != nil {
		t.Fatalf("reset: %v", err)
	}
	var r shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "command": "echo clean-output",
	}, &r); err != nil {
		t.Fatalf("command: %v", err)
	}
	if !strings.Contains(r.Output, "clean-output") {
		t.Fatalf("output missing: %q", r.Output)
	}
	if strings.Contains(r.Output, "BANNER-NOISE") || strings.Contains(r.Output, "HOOK-NOISE") {
		t.Fatalf("rc noise leaked into output: %q", r.Output)
	}
	if r.ShellDegraded {
		t.Fatalf("noisy (not broken) rc should not degrade: %+v", r)
	}
	_, _ = mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/root/.bashrc", "data": "",
	})
}

// --- file tools on the transparent session -------------------------------------

// fileRead is the ssh_file_read JSON result.
type fileRead struct {
	Data       string `json:"data"`
	StartLine  int    `json:"start_line"`
	Lines      int    `json:"lines"`
	TotalLines int    `json:"total_lines"`
	Truncated  bool   `json:"truncated"`
}

// fileEdit is the ssh_file_edit JSON result.
type fileEdit struct {
	StartLine  int `json:"start_line"`
	TotalLines int `json:"total_lines"`
}

func TestFileToolsUseHost(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	content := "line one\nline two\n"
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt", "data": content,
	}); err != nil {
		t.Fatalf("file_write: %v", err)
	}

	// A small text write then a read returns the new line-window result.
	var rd fileRead
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt",
	}, &rd); err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if rd.Data != content {
		t.Fatalf("round-trip mismatch: %q", rd.Data)
	}
	if rd.StartLine != 1 || rd.Lines != 2 || rd.TotalLines != 2 {
		t.Fatalf("line window = %+v, want start_line 1, lines 2, total_lines 2", rd)
	}
	if rd.Truncated {
		t.Fatalf("whole-file read marked truncated: %+v", rd)
	}

	// The edit lands where the read said it would, and the guard holds the
	// whole file rather than the text that came back.
	var ed fileEdit
	if err := mc.CallJSON(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt", "old_text": "line one", "new_text": "line 1",
	}, &ed); err != nil {
		t.Fatalf("file_edit: %v", err)
	}
	if ed.StartLine != 1 || ed.TotalLines != 2 {
		t.Fatalf("edit result = %+v, want start_line 1, total_lines 2", ed)
	}
	var after fileRead
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt",
	}, &after); err != nil {
		t.Fatalf("file_read after edit: %v", err)
	}
	if after.Data != "line 1\nline two\n" {
		t.Fatalf("edited file = %q", after.Data)
	}

	var entries []struct {
		Name string `json:"name"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_list", map[string]any{"host": "testhost", "path": "/tmp"}, &entries); err != nil {
		t.Fatalf("file_list: %v", err)
	}
	found := false
	for _, e := range entries {
		if e.Name == "e2e.txt" {
			found = true
		}
	}
	if !found {
		t.Fatal("file_list missing e2e.txt")
	}
}

// TestFileEditRejectsStaleRead needs a real remote change between the read and
// the edit, so it belongs here rather than on the in-process seams: the daemon
// must see content that no longer matches what it recorded.
func TestFileEditRejectsStaleRead(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	const path = "/tmp/e2e-stale.txt"
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": path, "data": "alpha\nbeta\n",
	}); err != nil {
		t.Fatalf("file_write: %v", err)
	}
	var rd fileRead
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": path,
	}, &rd); err != nil {
		t.Fatalf("file_read: %v", err)
	}

	// Someone else changes the file through the shell.
	run(t, mc, "testhost", "echo changed > "+path)

	if _, err := mc.CallText(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": path, "old_text": "alpha", "new_text": "ALPHA",
	}); err == nil || !strings.Contains(err.Error(), "ssh_file_read") {
		t.Fatalf("edit after an out-of-band change = %v, want an error naming ssh_file_read", err)
	}

	// Reading again makes the same edit legal, and it lands.
	var again fileRead
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": path,
	}, &again); err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if again.Data != "changed\n" {
		t.Fatalf("out-of-band change not visible: %q", again.Data)
	}
	if _, err := mc.CallText(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": path, "old_text": "changed", "new_text": "CHANGED",
	}); err != nil {
		t.Fatalf("edit after a fresh read: %v", err)
	}
	var after fileRead
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": path,
	}, &after); err != nil {
		t.Fatalf("file_read after edit: %v", err)
	}
	if after.Data != "CHANGED\n" {
		t.Fatalf("edited file = %q", after.Data)
	}
}

// TestShellWithoutBash pins the degraded path: a host with no bash must still
// run commands, report their exit status and keep state between calls. The
// fallback shell has no PROMPT_COMMAND, so the completion marker rides in PS1 —
// without that, every command runs to its timeout and reports status "running",
// which is what an ssh_shell call on a minimal host used to do.
func TestShellWithoutBash(t *testing.T) {
	t.Parallel()
	mc, _, d, teardown := setupDaemonImage(t, false)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var r shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "command": "command -v bash || echo no-bash-at-all; pwd", "timeout_ms": 20000,
	}, &r); err != nil {
		t.Logf("daemon stderr:\n%s", d.stderr.String())
		t.Fatalf("ssh_shell on a bash-less host: %v", err)
	}
	if r.Status != "done" {
		t.Fatalf("status=%q: the fallback shell never printed a marker", r.Status)
	}
	if !strings.Contains(r.Output, "no-bash-at-all") {
		t.Fatalf("fixture has bash or output is wrong: %q", r.Output)
	}
	if r.ExitCode == nil || *r.ExitCode != 0 {
		t.Fatalf("exit_code=%v, want 0", r.ExitCode)
	}
	if r.CWD == "" {
		t.Fatalf("cwd missing on the fallback shell: %+v", r)
	}
	if !r.ShellDegraded {
		t.Fatalf("expected shell_degraded on a bash-less host: %+v", r)
	}

	// Exit statuses come through the same path, and cd/env still persist.
	var st shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "command": "cd /tmp; MARK=fallback", "timeout_ms": 20000,
	}, &st); err != nil {
		t.Fatalf("cd on the fallback shell: %v", err)
	}
	if st.Status != "done" || st.CWD != "/tmp" {
		t.Fatalf("cd did not land: %+v", st)
	}
	var again shellResp
	if err := mc.CallJSON(ctx, "ssh_shell", map[string]any{
		"host": "testhost", "command": "echo $MARK; exit 3", "timeout_ms": 20000,
	}, &again); err != nil {
		t.Fatalf("second command on the fallback shell: %v", err)
	}
	if again.Status != "done" {
		t.Fatalf("status=%q after exit: %+v", again.Status, again)
	}
	if !strings.Contains(again.Output, "fallback") {
		t.Fatalf("state lost on the fallback shell: %+v", again)
	}
	if again.ExitCode == nil || *again.ExitCode != 3 {
		t.Fatalf("exit_code=%v after 'exit 3', want 3", again.ExitCode)
	}
}

// --- path spelling and cold start ---------------------------------------------

// TestTildePathsResolveToHome covers the path an agent naturally writes: "~",
// "~/x" and the plain relative form must name the same files as the absolute
// path, and a read spelled one way must license an edit spelled another (the
// read-before-edit guard keys on the resolved path).
func TestTildePathsResolveToHome(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The fixture sshd logs in as root, so the SFTP session starts in /root.
	// No ssh_shell here on purpose: this test is about the file tools, and the
	// "~" listing below checks this path rather than taking it on faith.
	home := "/" + sshUser
	const name = "e2e-tilde.txt"
	const body = "home sweet home\n"

	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "~/" + name, "data": body,
	}); err != nil {
		t.Fatalf("file_write via ~: %v", err)
	}

	var st struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_stat", map[string]any{
		"host": "testhost", "path": "~/" + name,
	}, &st); err != nil {
		t.Fatalf("file_stat via ~: %v", err)
	}
	if st.Name != name || st.Size != int64(len(body)) {
		t.Fatalf("stat via ~ = %+v, want name %q size %d", st, name, len(body))
	}

	// "~", the absolute path and the SFTP session's own cwd (the same home)
	// all list the file.
	for _, p := range []string{"~", "~/", home, ".", "./"} {
		var entries []struct {
			Name string `json:"name"`
		}
		if err := mc.CallJSON(ctx, "ssh_file_list", map[string]any{
			"host": "testhost", "path": p,
		}, &entries); err != nil {
			t.Fatalf("file_list %q: %v", p, err)
		}
		found := false
		for _, e := range entries {
			if e.Name == name {
				found = true
			}
		}
		if !found {
			t.Fatalf("file_list %q did not show %s", p, name)
		}
	}

	// Read through "~", edit through the absolute path.
	var rd fileRead
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "~/" + name,
	}, &rd); err != nil {
		t.Fatalf("file_read via ~: %v", err)
	}
	if _, err := mc.CallText(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": home + "/" + name, "old_text": "sweet", "new_text": "bitter",
	}); err != nil {
		t.Fatalf("edit via the absolute spelling of a read made with ~: %v", err)
	}

	// And the other way round: read absolute, edit via "~".
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": home + "/" + name,
	}, &rd); err != nil {
		t.Fatalf("file_read absolute: %v", err)
	}
	if _, err := mc.CallText(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": "~/" + name, "old_text": "bitter", "new_text": "sweet",
	}); err != nil {
		t.Fatalf("edit via ~ of a read made absolute: %v", err)
	}

	// "~user" is not our home to resolve: it stays a literal path and fails
	// as a path, not as a surprising success.
	if _, err := mc.CallText(ctx, "ssh_file_stat", map[string]any{
		"host": "testhost", "path": "~e2e-no-such-user/" + name,
	}); err == nil {
		t.Fatal("~user path resolved instead of staying literal")
	}
}

// TestConcurrentFirstCallsShareOneDial pins the cold-start gate: the session
// is published before its dial finishes, so parallel first calls must wait for
// it — none may fail with "session is connecting" — and the host must end up
// with exactly one connection.
func TestConcurrentFirstCallsShareOneDial(t *testing.T) {
	t.Parallel()
	_, _, d, teardown := setupDaemon(t)
	defer teardown()

	// setupDaemon exposes the host over /api and never dials: still cold.
	const callers = 6
	clients := make([]*mcptest.Client, callers)
	for i := range clients {
		clients[i] = mcptest.New(t, d.Addr())
	}
	defer func() {
		for _, c := range clients {
			c.Close()
		}
	}()

	var wg sync.WaitGroup
	errs := make([]error, callers)
	start := make(chan struct{})
	for i := range clients {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_, errs[i] = clients[i].CallTool(ctx, "ssh_file_list", map[string]any{
				"host": "testhost", "path": "/",
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("parallel first call %d: %v", i, err)
		}
	}

	var sess []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if code := apiJSON(t, d, "GET", "/sessions", nil, &sess); code != 200 {
		t.Fatalf("GET /api/sessions = %d", code)
	}
	if len(sess) != 1 {
		t.Fatalf("%d sessions after %d parallel first calls, want 1: %+v", len(sess), callers, sess)
	}
	if sess[0].State != "connected" {
		t.Fatalf("session state = %q, want connected", sess[0].State)
	}
}

// --- state file hygiene --------------------------------------------------------

func TestStateFileHasNoSessions(t *testing.T) {
	t.Parallel()
	mc, _, d, _ := setupDaemon(t)

	run(t, mc, "testhost", "echo x")
	d.Stop() // flushes state
	data, err := os.ReadFile(d.StatePath())
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if strings.Contains(string(data), "ssh_sessions") || strings.Contains(string(data), "IdentityFile") || strings.Contains(string(data), "id_ed25519") {
		t.Fatalf("state file leaked session/credential material: %s", data)
	}
}
