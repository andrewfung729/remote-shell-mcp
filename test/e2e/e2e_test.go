//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
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

// shellResp is the ssh_shell status line, parsed for assertions. The text
// block is the output plus one bracketed line; there is no JSON result.
type shellResp struct {
	Output         string
	Status         string
	ExitCode       *int
	CWD            string
	Truncated      bool
	Shell          string
	ShellRestarted bool
	ShellDegraded  bool
}

func shellCall(t *testing.T, mc *mcptest.Client, ctx context.Context, args map[string]any) (shellResp, error) {
	t.Helper()
	text, err := mc.CallText(ctx, "ssh_shell", args)
	if err != nil {
		return shellResp{}, err
	}
	return parseShell(text), nil
}

func mustText(t *testing.T, mc *mcptest.Client, ctx context.Context, tool string, args map[string]any) string {
	t.Helper()
	text, err := mc.CallText(ctx, tool, args)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return text
}

// parseShell splits a tool result into output and the trailing status line.
func parseShell(text string) shellResp {
	body := text
	line := ""
	if i := strings.LastIndex(text, "\n["); i >= 0 && strings.HasSuffix(strings.TrimRight(text, "\n"), "]") {
		body = text[:i]
		line = strings.TrimRight(text[i+1:], "\n")
	} else if strings.HasPrefix(text, "[") && strings.HasSuffix(strings.TrimRight(text, "\n"), "]") && !strings.Contains(strings.TrimRight(text, "\n"), "\n") {
		body = ""
		line = strings.TrimRight(text, "\n")
	}
	r := shellResp{Output: body}
	if line == "" {
		return r
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
	for _, seg := range strings.Split(inner, " · ") {
		switch {
		case seg == "running — poll to continue":
			r.Status = "running"
		case strings.HasPrefix(seg, "exit "):
			r.Status = "done"
			n, _ := strconv.Atoi(strings.TrimPrefix(seg, "exit "))
			r.ExitCode = &n
		case strings.HasPrefix(seg, "cwd "):
			r.CWD = strings.TrimPrefix(seg, "cwd ")
		case seg == "truncated, poll for full output":
			r.Truncated = true
		case seg == "shell restarted: cwd/env lost":
			r.ShellRestarted = true
		case seg == "degraded: sh fallback":
			r.ShellDegraded = true
		case strings.HasPrefix(seg, "shell "):
			r.Shell = strings.TrimPrefix(seg, "shell ")
		}
	}
	if r.Status == "" {
		r.Status = "done"
	}
	return r
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
	r, err := shellCall(t, mc, ctx, map[string]any{"host": host, "command": cmd})
	if err != nil {
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
	if r.Shell != "" {
		t.Fatalf("default shell named in the status line: %+v", r)
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

	done := make(chan shellResp, 1)
	go func() {
		r, _ := shellCall(t, mc, ctx, map[string]any{
			"host": "testhost", "shell": "build", "command": "sleep 3; echo build-done", "timeout_ms": 15000,
		})
		done <- r
	}()
	time.Sleep(500 * time.Millisecond)
	r := run(t, mc, "testhost", "echo default-shell-works")
	build := <-done
	if !strings.Contains(r.Output, "default-shell-works") {
		t.Fatalf("default shell blocked: %q", r.Output)
	}
	if build.Shell != "build" || build.Status != "done" || !strings.Contains(build.Output, "build-done") {
		t.Fatalf("named shell status = %+v", build)
	}
}

func TestShellTimeoutThenPoll(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	r, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "sleep 2; echo late-done", "timeout_ms": 300,
	})
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if r.Status != "running" {
		t.Fatalf("status=%q want running — poll to continue", r.Status)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		p, err := shellCall(t, mc, ctx, map[string]any{
			"host": "testhost", "poll": true, "timeout_ms": 5000,
		})
		if err != nil {
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
		_, _ = shellCall(t, mc, ctx, map[string]any{
			"host": "testhost", "command": "sleep 5; echo x", "timeout_ms": 10000,
		})
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

	// The command returns running; interrupt itself reports whether Ctrl-C
	// ended it, including the exit code.
	if _, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "sleep 60; echo never", "timeout_ms": 400,
	}); err != nil {
		t.Fatalf("command: %v", err)
	}
	p, err := shellCall(t, mc, ctx, map[string]any{"host": "testhost", "interrupt": true, "timeout_ms": 8000})
	if err != nil {
		t.Fatalf("interrupt: %v", err)
	}
	if p.Status != "done" {
		t.Fatalf("status=%q — interrupt did not kill the command", p.Status)
	}
	if strings.Contains(p.Output, "never") {
		t.Fatalf("interrupted command ran to completion: %q", p.Output)
	}
	if p.ExitCode == nil || *p.ExitCode == 0 {
		t.Fatalf("expected non-zero exit after interrupt, got %v", p.ExitCode)
	}
}

func TestShellInputInteractive(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The prompt is still waiting when the command call times out. input
	// answers it and returns the program's reply in the same call.
	if _, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "read -p ANSWER: ans; echo got-$ans", "timeout_ms": 800,
	}); err != nil {
		t.Fatalf("command: %v", err)
	}
	p, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "input": "hello-input\n", "timeout_ms": 8000,
	})
	if err != nil {
		t.Fatalf("input: %v", err)
	}
	if p.Status != "done" || !strings.Contains(p.Output, "got-hello-input") {
		t.Fatalf("input did not return the reply: %+v", p)
	}
}

func TestShellReset(t *testing.T) {
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()

	run(t, mc, "testhost", "cd /tmp; MARKER=set")
	text, err := mc.CallText(context.Background(), "ssh_shell", map[string]any{"host": "testhost", "reset": true})
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if text != "[shell restarted: cwd/env lost]" {
		t.Fatalf("reset status = %q", text)
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
		res, err := shellCall(t, mc, ctx, map[string]any{
			"host": "testhost", "command": "echo after-kill", "timeout_ms": 15000,
		})
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
	if _, err := shellCall(t, mc, ctx, map[string]any{"host": "testhost", "reset": true}); err != nil {
		t.Fatalf("reset after broken rc: %v", err)
	}
	r2, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "echo still-works",
	})
	if err != nil {
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
	r, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "echo clean-output",
	})
	if err != nil {
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
	t.Parallel()
	mc, _, _, teardown := setupDaemon(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	content := "line one\nline two\n"
	wrote := mustText(t, mc, ctx, "ssh_file_write", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt", "data": content,
	})
	if wrote != fmt.Sprintf("wrote %d bytes to /tmp/e2e.txt", len(content)) {
		t.Fatalf("file_write = %q", wrote)
	}

	rd := mustText(t, mc, ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt",
	})
	if rd != content+"[lines 1–2 of 2]" {
		t.Fatalf("file_read = %q", rd)
	}

	ed := mustText(t, mc, ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt", "old_text": "line one", "new_text": "line 1",
	})
	if ed != "edited line 1 (file now 2 lines)" {
		t.Fatalf("file_edit = %q", ed)
	}
	after := mustText(t, mc, ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "/tmp/e2e.txt",
	})
	if after != "line 1\nline two\n[lines 1–2 of 2]" {
		t.Fatalf("edited file = %q", after)
	}

	listed := mustText(t, mc, ctx, "ssh_file_list", map[string]any{"host": "testhost", "path": "/tmp"})
	if !strings.Contains(listed, " e2e.txt\n") && !strings.HasSuffix(listed, " e2e.txt") {
		t.Fatalf("file_list missing e2e.txt: %q", listed)
	}
	if strings.Contains(listed, "e2e.txt/") {
		t.Fatalf("file listed as a directory: %q", listed)
	}
	st := mustText(t, mc, ctx, "ssh_file_stat", map[string]any{"host": "testhost", "path": "/tmp/e2e.txt"})
	if !strings.Contains(st, "name: e2e.txt\n") || !strings.Contains(st, "is_dir: false") {
		t.Fatalf("file_stat = %q", st)
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
	if _, err := mc.CallText(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": path,
	}); err != nil {
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
	again := mustText(t, mc, ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": path,
	})
	if !strings.HasPrefix(again, "changed\n") {
		t.Fatalf("out-of-band change not visible: %q", again)
	}
	if _, err := mc.CallText(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": path, "old_text": "changed", "new_text": "CHANGED",
	}); err != nil {
		t.Fatalf("edit after a fresh read: %v", err)
	}
	after := mustText(t, mc, ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": path,
	})
	if !strings.HasPrefix(after, "CHANGED\n") {
		t.Fatalf("edited file = %q", after)
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

	r, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "command -v bash || echo no-bash-at-all; pwd", "timeout_ms": 20000,
	})
	if err != nil {
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
	st, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "cd /tmp; MARK=fallback", "timeout_ms": 20000,
	})
	if err != nil {
		t.Fatalf("cd on the fallback shell: %v", err)
	}
	if st.Status != "done" || st.CWD != "/tmp" {
		t.Fatalf("cd did not land: %+v", st)
	}
	again, err := shellCall(t, mc, ctx, map[string]any{
		"host": "testhost", "command": "echo $MARK; exit 3", "timeout_ms": 20000,
	})
	if err != nil {
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

	st := mustText(t, mc, ctx, "ssh_file_stat", map[string]any{
		"host": "testhost", "path": "~/" + name,
	})
	if !strings.Contains(st, "name: "+name+"\n") || !strings.Contains(st, fmt.Sprintf("size: %d\n", len(body))) {
		t.Fatalf("stat via ~ = %q", st)
	}

	// "~", the absolute path and the SFTP session's own cwd (the same home)
	// all list the file.
	for _, p := range []string{"~", "~/", home, ".", "./"} {
		listed := mustText(t, mc, ctx, "ssh_file_list", map[string]any{
			"host": "testhost", "path": p,
		})
		if !strings.Contains(listed, " "+name+"\n") && !strings.HasSuffix(listed, " "+name) {
			t.Fatalf("file_list %q did not show %s: %q", p, name, listed)
		}
	}

	// Read through "~", edit through the absolute path.
	if _, err := mc.CallText(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": "~/" + name,
	}); err != nil {
		t.Fatalf("file_read via ~: %v", err)
	}
	if _, err := mc.CallText(ctx, "ssh_file_edit", map[string]any{
		"host": "testhost", "path": home + "/" + name, "old_text": "sweet", "new_text": "bitter",
	}); err != nil {
		t.Fatalf("edit via the absolute spelling of a read made with ~: %v", err)
	}

	// And the other way round: read absolute, edit via "~".
	if _, err := mc.CallText(ctx, "ssh_file_read", map[string]any{
		"host": "testhost", "path": home + "/" + name,
	}); err != nil {
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
