//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/mcp"
)

func TestMain_BinariesAndImage(t *testing.T) {
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
func setupDaemon(t *testing.T) (*MCPClient, *SSHDContainer, *Daemon, func()) {
	t.Helper()
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	home := testHomeForContainer(t, sshd, hostBlock(t, "testhost", sshd.SSHPort))
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{Env: []string{"HOME=" + home}})
	exchangeToken(t, daemon)
	exposeHost(t, daemon, "testhost")
	mc := NewMCPClientForDaemon(t, daemon)
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
func run(t *testing.T, mc *MCPClient, host, cmd string) shellResp {
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
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()

	run(t, mc, "testhost", "MARK=abc123; cd /tmp")
	r := run(t, mc, "testhost", "echo $MARK $PWD")
	if !strings.Contains(r.Output, "abc123 /tmp") {
		t.Fatalf("state lost: %q", r.Output)
	}
}

func TestShellNamedParallel(t *testing.T) {
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
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		var r shellResp
		_ = mc.CallJSON(ctx, "ssh_shell", map[string]any{
			"host": "testhost", "command": "read -p ANSWER: ans; echo got-$ans", "timeout_ms": 15000,
		}, &r)
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
	if !strings.Contains(p.Output, "got-hello-input") {
		t.Fatalf("interactive input did not reach the command: %q", p.Output)
	}
}

func TestShellReset(t *testing.T) {
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

func TestUnexposedHostRejected(t *testing.T) {
	mc, _, d, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	_, err := mc.CallText(ctx, "ssh_shell", map[string]any{"host": "ghosthost", "command": "id"})
	if err == nil || !strings.Contains(err.Error(), "host not exposed") {
		t.Fatalf("unexposed host not refused: %v", err)
	}
	_, err = mc.CallText(ctx, "ssh_file_list", map[string]any{"host": "ghosthost", "path": "/"})
	if err == nil || !strings.Contains(err.Error(), "host not exposed") {
		t.Fatalf("file tool on unexposed host not refused: %v", err)
	}
	unexposeHost(t, d, "testhost")
	_, err2 := mc.CallText(ctx, "ssh_shell", map[string]any{"host": "testhost", "command": "id"})
	if err2 == nil || !strings.Contains(err2.Error(), "host not exposed") {
		t.Fatalf("revoked host not refused: %v", err2)
	}
	exposeHost(t, d, "testhost")
}

func TestRevokeClosesSessionAndShells(t *testing.T) {
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

func TestFileToolsUseHost(t *testing.T) {
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	content := "host-keyed file tools" + string(byte(10))
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt", "data": content,
	}); err != nil {
		t.Fatalf("file_write: %v", err)
	}
	var rd struct {
		Data string `json:"data"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt",
	}, &rd); err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if rd.Data != content {
		t.Fatalf("round-trip mismatch: %q", rd.Data)
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

// --- tools/list surface ----------------------------------------------------------

func TestToolListSurface(t *testing.T) {
	mc, _, d, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	listTools := func() map[string]bool {
		res, err := mc.c.ListTools(ctx, mcp.ListToolsRequest{})
		if err != nil {
			t.Fatalf("tools/list: %v", err)
		}
		out := map[string]bool{}
		for _, tt := range res.Tools {
			out[tt.Name] = true
		}
		return out
	}

	tools := listTools()
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

	unexposeHost(t, d, "testhost")
	tools = listTools()
	if tools["ssh_hosts"] || tools["ssh_shell"] {
		t.Fatalf("gated tools visible with no exposed host: %v", tools)
	}
	exposeHost(t, d, "testhost")
	tools = listTools()
	if !tools["ssh_hosts"] || !tools["ssh_shell"] {
		t.Fatalf("gated tools not restored after re-expose: %v", tools)
	}
}

// --- state file hygiene --------------------------------------------------------

func TestStateFileHasNoSessions(t *testing.T) {
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
