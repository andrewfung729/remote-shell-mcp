//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
)

func TestMain_BinariesAndImage(t *testing.T) {
	_ = BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
}

type sshConnectResp struct {
	ID            string   `json:"id"`
	State         string   `json:"state"`
	ActiveAddress string   `json:"active_address"`
	Addresses     []string `json:"addresses"`
}

type execResp struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exit_code"`
}

func sshSpec(t *testing.T, port int) map[string]any {
	t.Helper()
	return map[string]any{
		"user":               sshUser,
		"addresses":          []string{fmt.Sprintf("127.0.0.1:%d", port)},
		"auth":               keyAuth(t),
		"insecure":           true,
		"timeout":            "10s",
		"disable_ssh_config": true,
	}
}

func setupSession(t *testing.T) (*MCPClient, *SSHDContainer, *Daemon, func()) {
	t.Helper()
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	daemon := StartDaemon(t, daemonBin)
	mc := NewMCPClientForDaemon(t, daemon)
	teardown := func() {
		mc.Close()
		daemon.Stop()
		sshd.Stop()
	}
	return mc, sshd, daemon, teardown
}

func TestSSHConnectAndExec(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{
		"id":   "s1",
		"spec": sshSpec(t, sshd.SSHPort),
	}, &info); err != nil {
		t.Fatalf("ssh_connect: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("expected state=connected, got %q", info.State)
	}

	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "s1",
		"command":    "echo hello-from-container && hostname",
	}, &res); err != nil {
		t.Fatalf("ssh_exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "hello-from-container") {
		t.Fatalf("exec stdout missing token: %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("expected exit_code=0, got %d (stderr=%q)", res.ExitCode, res.Stderr)
	}
}

func TestSSHPersistentShell(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_shell_open", map[string]any{"session_id": "s1", "shell_id": "sh1"}); err != nil {
		t.Fatalf("shell_open: %v", err)
	}
	// drain banner
	_, _ = mc.Call(ctx, "ssh_shell_read", map[string]any{"session_id": "s1", "shell_id": "sh1", "timeout_ms": 800})

	if _, err := mc.Call(ctx, "ssh_shell_write", map[string]any{"session_id": "s1", "shell_id": "sh1", "data": "MARK=abc123; cd /tmp; pwd; echo $MARK\n"}); err != nil {
		t.Fatalf("shell_write: %v", err)
	}

	got := readUntil(t, mc, "s1", "sh1", "abc123", 3*time.Second)
	if !strings.Contains(got, "/tmp") {
		t.Fatalf("shell did not preserve cwd; output:\n%s", got)
	}
	if !strings.Contains(got, "abc123") {
		t.Fatalf("shell did not preserve env var; output:\n%s", got)
	}

	if _, err := mc.Call(ctx, "ssh_shell_write", map[string]any{"session_id": "s1", "shell_id": "sh1", "data": "echo $MARK $PWD\n"}); err != nil {
		t.Fatalf("shell_write 2: %v", err)
	}
	got2 := readUntil(t, mc, "s1", "sh1", "abc123 /tmp", 3*time.Second)
	if !strings.Contains(got2, "abc123 /tmp") {
		t.Fatalf("state lost between calls; output:\n%s", got2)
	}

	if _, err := mc.Call(ctx, "ssh_shell_close", map[string]any{"session_id": "s1", "shell_id": "sh1"}); err != nil {
		t.Fatalf("shell_close: %v", err)
	}
}

func readUntil(t *testing.T, mc *MCPClient, sid, shid, needle string, total time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), total+2*time.Second)
	defer cancel()
	var acc strings.Builder
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		var out struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		if err := mc.CallJSON(ctx, "ssh_shell_read", map[string]any{
			"session_id": sid, "shell_id": shid, "timeout_ms": 400,
		}, &out); err != nil {
			t.Fatalf("shell_read: %v", err)
		}
		acc.WriteString(out.Data)
		if strings.Contains(acc.String(), needle) {
			return acc.String()
		}
		if out.EOF {
			return acc.String()
		}
	}
	return acc.String()
}

func TestSFTP(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "s1", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	content := "hello sftp from e2e\n"
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"session_id": "s1", "path": "/tmp/e2e.txt", "data": content,
	}); err != nil {
		t.Fatalf("file_write: %v", err)
	}
	var rd struct {
		Bytes int    `json:"bytes"`
		Data  string `json:"data"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"session_id": "s1", "path": "/tmp/e2e.txt",
	}, &rd); err != nil {
		t.Fatalf("file_read: %v", err)
	}
	if rd.Data != content {
		t.Fatalf("round-trip mismatch: wrote %q got %q", content, rd.Data)
	}
	var entries []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_list", map[string]any{
		"session_id": "s1", "path": "/tmp",
	}, &entries); err != nil {
		t.Fatalf("file_list: %v", err)
	}
	var found bool
	for _, e := range entries {
		if e.Name == "e2e.txt" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("file_list did not include e2e.txt; got %d entries", len(entries))
	}
}

// waitForContainerHTTP confirms the busybox httpd inside the container is serving.
func waitForContainerHTTP(t *testing.T, sshd *SSHDContainer) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "exec", sshd.ID, "wget", "-qO-", "http://127.0.0.1:8080/marker.txt").Output()
		if err == nil && strings.Contains(string(out), "HELLO-FROM-IN-CONTAINER") {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("in-container httpd never came up on :8080")
}

// TestSFTPFullCoverage exercises the SFTP tool surface that TestSFTP doesn't —
// chmod, mkdir, delete, rename, upload, download (incl. directory recursion),
// and stat.
func TestSFTPFullCoverage(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "f1", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// mkdir (recursive)
	if _, err := mc.Call(ctx, "ssh_file_mkdir", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b", "recursive": true,
	}); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// write a file under the new directory
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/file.txt", "data": "first\n",
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	// chmod via string (the new API)
	if _, err := mc.Call(ctx, "ssh_file_chmod", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/file.txt", "mode": "0640",
	}); err != nil {
		t.Fatalf("chmod string: %v", err)
	}
	// stat confirms the mode (output mode string contains rw-r-----)
	var st struct {
		Mode string `json:"mode"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_stat", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/file.txt",
	}, &st); err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !strings.Contains(st.Mode, "rw-r-----") {
		t.Fatalf("chmod 0640 did not produce expected mode; got %q", st.Mode)
	}

	// rename
	if _, err := mc.Call(ctx, "ssh_file_rename", map[string]any{
		"session_id": "f1",
		"from":       "/tmp/e2e-sftp/a/b/file.txt",
		"to":         "/tmp/e2e-sftp/a/b/renamed.txt",
	}); err != nil {
		t.Fatalf("rename: %v", err)
	}

	// upload + download round-trip via local temp files
	localDir := t.TempDir()
	srcPath := localDir + "/up.bin"
	dstPath := localDir + "/down.bin"
	srcContent := []byte("upload payload " + strings.Repeat("x", 1024))
	if err := os.WriteFile(srcPath, srcContent, 0o600); err != nil {
		t.Fatalf("local write: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_upload", map[string]any{
		"session_id":  "f1",
		"local_path":  srcPath,
		"remote_path": "/tmp/e2e-sftp/uploaded.bin",
	}); err != nil {
		t.Fatalf("upload: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_download", map[string]any{
		"session_id":  "f1",
		"remote_path": "/tmp/e2e-sftp/uploaded.bin",
		"local_path":  dstPath,
	}); err != nil {
		t.Fatalf("download: %v", err)
	}
	roundtrip, err := os.ReadFile(dstPath)
	if err != nil {
		t.Fatalf("local read: %v", err)
	}
	if string(roundtrip) != string(srcContent) {
		t.Fatalf("upload/download did not round-trip (%d vs %d bytes)", len(roundtrip), len(srcContent))
	}

	// delete
	if _, err := mc.Call(ctx, "ssh_file_delete", map[string]any{
		"session_id": "f1", "path": "/tmp/e2e-sftp/a/b/renamed.txt",
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Directory round-trip: build a small local tree, upload it as a directory,
	// then download it back to a fresh dir. Confirms the recursive walk plus
	// remote MkdirAll + file streaming + ToSlash handling all line up.
	treeRoot := filepath.Join(localDir, "tree")
	for rel, body := range map[string]string{
		"a.txt":         "alpha",
		"sub/b.txt":     "bravo",
		"sub/deep/c.go": "package x",
	} {
		full := filepath.Join(treeRoot, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := mc.Call(ctx, "ssh_upload", map[string]any{
		"session_id":  "f1",
		"local_path":  treeRoot,
		"remote_path": "/tmp/e2e-sftp/tree-up",
	}); err != nil {
		t.Fatalf("upload dir: %v", err)
	}
	downRoot := filepath.Join(localDir, "tree-down")
	if _, err := mc.Call(ctx, "ssh_download", map[string]any{
		"session_id":  "f1",
		"remote_path": "/tmp/e2e-sftp/tree-up",
		"local_path":  downRoot,
	}); err != nil {
		t.Fatalf("download dir: %v", err)
	}
	for rel, want := range map[string]string{
		"a.txt":         "alpha",
		"sub/b.txt":     "bravo",
		"sub/deep/c.go": "package x",
	} {
		got, err := os.ReadFile(filepath.Join(downRoot, rel))
		if err != nil {
			t.Fatalf("read %s after dir round-trip: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s: got %q want %q", rel, got, want)
		}
	}
}

// TestSSHJumpHost exercises ProxyJump-style multi-hop: connect via the
// container's sshd as a jump, then SSH to 127.0.0.1:22 INSIDE the container
// (which loops back through the same sshd). Both hops authenticate with the
// suite key installed into the container at start.
func TestSSHJumpHost(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec := map[string]any{
		"user": sshUser,
		// Final target is reached via the jump's perspective, so 127.0.0.1:22
		// resolves on the jump host (= the container itself).
		"addresses": []string{"127.0.0.1:22"},
		"auth":      keyAuth(t),
		"insecure":  true,
		"jump_hosts": []map[string]any{
			{
				"user":      sshUser,
				"addresses": []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
				"auth":      keyAuth(t),
				"insecure":  true,
			},
		},
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "jh", "spec": spec}); err != nil {
		t.Fatalf("ssh_connect via jump: %v", err)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "jh", "command": "echo through-bastion-$(hostname)",
	}, &res); err != nil {
		t.Fatalf("ssh_exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "through-bastion") {
		t.Fatalf("jump-host exec stdout: %q", res.Stdout)
	}
}

// TestSSHMultiAddress confirms ssh_connect tries addresses in order: an
// unreachable address first, the real container second.
func TestSSHMultiAddress(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	// 127.0.0.1:1 is essentially always closed; daemon should fall through.
	spec := map[string]any{
		"user":      sshUser,
		"addresses": []string{"127.0.0.1:1", fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":      keyAuth(t),
		"insecure":  true,
		"timeout":   "2s",
	}
	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{"id": "ma", "spec": spec}, &info); err != nil {
		t.Fatalf("ssh_connect multi-address: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("state %q", info.State)
	}
	if !strings.HasSuffix(info.ActiveAddress, fmt.Sprintf(":%d", sshd.SSHPort)) {
		t.Fatalf("expected active_address to be the real sshd port; got %q", info.ActiveAddress)
	}
}

// TestSSHClone forks an existing session under a new id and confirms both
// answer exec independently.
func TestSSHClone(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "orig", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_clone", map[string]any{"source_id": "orig", "new_id": "twin"}); err != nil {
		t.Fatalf("clone: %v", err)
	}
	for _, id := range []string{"orig", "twin"} {
		var res execResp
		if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
			"session_id": id, "command": "echo " + id,
		}, &res); err != nil {
			t.Fatalf("exec on %s: %v", id, err)
		}
		if !strings.Contains(res.Stdout, id) {
			t.Fatalf("exec on %s returned %q", id, res.Stdout)
		}
	}
	// Closing one should not affect the other.
	if _, err := mc.Call(ctx, "ssh_disconnect", map[string]any{"id": "orig"}); err != nil {
		t.Fatalf("disconnect orig: %v", err)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "twin", "command": "echo still-alive",
	}, &res); err != nil {
		t.Fatalf("twin exec after orig disconnect: %v", err)
	}
	if !strings.Contains(res.Stdout, "still-alive") {
		t.Fatalf("twin output %q", res.Stdout)
	}
}

// TestSSHKeyAuth exercises per-spec key auth: a fresh ephemeral keypair
// (not the baked-in suite key) installed into the container at test time.
func TestSSHKeyAuth(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	privPath, pub := GenerateKey(t)
	InstallAuthorizedKey(t, sshd, pub)

	spec := map[string]any{
		"user":      sshUser,
		"addresses": []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":      map[string]any{"key_path": privPath},
		"insecure":  true,
		"timeout":   "10s",
	}
	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{"id": "k1", "spec": spec}, &info); err != nil {
		t.Fatalf("ssh_connect key: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("state = %q", info.State)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "k1", "command": "whoami",
	}, &res); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "root") {
		t.Fatalf("whoami via key auth returned %q", res.Stdout)
	}
}

func TestSSHAgentAuth(t *testing.T) {
	if _, err := exec.LookPath("ssh-agent"); err != nil {
		t.Skip("ssh-agent not available")
	}
	if _, err := exec.LookPath("ssh-add"); err != nil {
		t.Skip("ssh-add not available")
	}
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()

	privPath, pub := GenerateKey(t)
	InstallAuthorizedKey(t, sshd, pub)
	authSock := StartSSHAgent(t, privPath)

	// Daemon must see SSH_AUTH_SOCK to talk to the agent.
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{Env: []string{"SSH_AUTH_SOCK=" + authSock}})
	defer daemon.Stop()

	mc := NewMCPClientForDaemon(t, daemon)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":      sshUser,
		"addresses": []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":      map[string]any{"use_agent": true},
		"insecure":  true,
		"timeout":   "10s",
	}
	var info sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_connect", map[string]any{"id": "a1", "spec": spec}, &info); err != nil {
		t.Fatalf("ssh_connect agent: %v", err)
	}
	if info.State != "connected" {
		t.Fatalf("agent auth state = %q", info.State)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
		"session_id": "a1", "command": "echo agent-ok",
	}, &res); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !strings.Contains(res.Stdout, "agent-ok") {
		t.Fatalf("exec output %q", res.Stdout)
	}
}

func TestSSHAutoReconnect(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":           sshUser,
		"addresses":      []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":           keyAuth(t),
		"insecure":       true,
		"timeout":        "10s",
		"keepalive":      "1s",
		"auto_reconnect": true,
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "r1", "spec": spec}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// Sanity check it works before we break it.
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "r1", "command": "echo before"}, &res); err != nil {
		t.Fatalf("exec pre-kill: %v", err)
	}
	if !strings.Contains(res.Stdout, "before") {
		t.Fatalf("pre-kill stdout %q", res.Stdout)
	}

	// Kill sshd inside the container. The respawn loop in the Dockerfile brings it back.
	if _, err := exec.Command("docker", "exec", sshd.ID, "sh", "-c", "pkill -KILL sshd").CombinedOutput(); err != nil {
		t.Fatalf("kill sshd: %v", err)
	}

	// Poll ssh_info until state goes through reconnecting/disconnected and back to connected.
	deadline := time.Now().Add(20 * time.Second)
	var sawReconnecting bool
	var finalState string
	for time.Now().Before(deadline) {
		var info sshConnectResp
		if err := mc.CallJSON(ctx, "ssh_info", map[string]any{"id": "r1"}, &info); err != nil {
			t.Fatalf("ssh_info: %v", err)
		}
		finalState = info.State
		if info.State == "reconnecting" || info.State == "disconnected" {
			sawReconnecting = true
		}
		if sawReconnecting && info.State == "connected" {
			break
		}
		time.Sleep(400 * time.Millisecond)
	}
	if !sawReconnecting {
		t.Fatalf("never observed reconnecting state; final=%q", finalState)
	}
	if finalState != "connected" {
		t.Fatalf("session did not recover; final state=%q", finalState)
	}

	// Verify exec works again on the new connection.
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "r1", "command": "echo after"}, &res); err != nil {
		t.Fatalf("exec post-reconnect: %v", err)
	}
	if !strings.Contains(res.Stdout, "after") {
		t.Fatalf("post-reconnect stdout %q", res.Stdout)
	}
}

func TestStateAcrossDaemonRestart(t *testing.T) {
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()

	stateDir := t.TempDir()
	statePath := stateDir + "/state.json"
	lockPath := stateDir + "/daemon.lock"

	// Phase 1 — connect a persistent session with the suite key.
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	mc := NewMCPClientForDaemon(t, daemon)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":       sshUser,
		"addresses":  []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":       keyAuth(t),
		"insecure":   true,
		"timeout":    "10s",
		"persistent": true,
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "persist-d", "spec": spec}); err != nil {
		mc.Close()
		daemon.Stop()
		t.Fatalf("connect: %v", err)
	}
	// Make sure the snapshot has actually been flushed.
	if _, err := mc.Call(ctx, "snapshot", nil); err != nil {
		mc.Close()
		daemon.Stop()
		t.Fatalf("snapshot: %v", err)
	}
	mc.Close()
	daemon.Stop()

	// Confirm state file is non-empty so the next daemon has something to rehydrate.
	if st, err := os.Stat(statePath); err != nil || st.Size() == 0 {
		t.Fatalf("state file empty after snapshot: stat=%v size=%v", err, st)
	}

	// Phase 2 — fresh daemon on a new port, same state file.
	daemon2 := StartDaemonWith(t, daemonBin, DaemonOpts{StatePath: statePath, LockPath: lockPath})
	defer daemon2.Stop()
	mc2 := NewMCPClientForDaemon(t, daemon2)
	defer mc2.Close()

	ctx2, cancel2 := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel2()

	// The persistent session should be back, reconnected.
	deadline := time.Now().Add(8 * time.Second)
	var found bool
	for time.Now().Before(deadline) {
		var list []sshConnectResp
		if err := mc2.CallJSON(ctx2, "ssh_list", nil, &list); err != nil {
			t.Fatalf("ssh_list: %v", err)
		}
		for _, s := range list {
			if s.ID == "persist-d" && s.State == "connected" {
				found = true
				break
			}
		}
		if found {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if !found {
		t.Fatalf("persistent session did not rehydrate on daemon 2")
	}

	// Exec to confirm the rehydrated session is actually usable.
	var res execResp
	if err := mc2.CallJSON(ctx2, "ssh_exec", map[string]any{
		"session_id": "persist-d", "command": "echo restored",
	}, &res); err != nil {
		t.Fatalf("exec after rehydrate: %v", err)
	}
	if !strings.Contains(res.Stdout, "restored") {
		t.Fatalf("rehydrated session exec stdout %q", res.Stdout)
	}
}

// TestManyParallelSessions opens N SSH sessions, runs a command on each, then
// closes them all. Verifies the manager doesn't deadlock under fanout and
// that close cleans up.
func TestManyParallelSessions(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const N = 10
	var wg sync.WaitGroup
	errs := make(chan error, N*3)

	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("p%d", i)
			if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": id, "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
				errs <- fmt.Errorf("%s connect: %w", id, err)
				return
			}
			var res execResp
			if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{
				"session_id": id, "command": fmt.Sprintf("echo session-%d", i),
			}, &res); err != nil {
				errs <- fmt.Errorf("%s exec: %w", id, err)
				return
			}
			want := fmt.Sprintf("session-%d", i)
			if !strings.Contains(res.Stdout, want) {
				errs <- fmt.Errorf("%s wrong output: %q", id, res.Stdout)
				return
			}
			if _, err := mc.Call(ctx, "ssh_disconnect", map[string]any{"id": id}); err != nil {
				errs <- fmt.Errorf("%s disconnect: %w", id, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}

	// ssh_list should be empty after all disconnects.
	var list []sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_list", nil, &list); err != nil {
		t.Fatalf("ssh_list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("expected 0 sessions after disconnect, got %d", len(list))
	}
}

// NOTE: parallel HTTP clients hitting the same daemon is covered by
// TestManyParallelSessions (10 concurrent ssh_connect on one client — the
// daemon side runs N handler goroutines), TestConcurrentShellWrites, and
// TestConcurrentReconnects.

// TestLargeMCPResponse drives a tool that returns a multi-megabyte payload
// to confirm /mcp handles big JSON-RPC bodies without a scanner/buffer cap.
func TestLargeMCPResponse(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "big", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	// 2 MiB file → forces a JSON-RPC response larger than the old 16 MiB cap is
	// not enough to trigger the bug, but any response > the default bufio buffer
	// would have stuck without our fix.
	const sz = 2 * 1024 * 1024
	mk := strings.Repeat("X", sz)
	if _, err := mc.Call(ctx, "ssh_file_write", map[string]any{
		"session_id": "big", "path": "/tmp/big.bin", "data": mk,
	}); err != nil {
		t.Fatalf("write big: %v", err)
	}
	var rd struct {
		Bytes int    `json:"bytes"`
		Data  string `json:"data"`
	}
	if err := mc.CallJSON(ctx, "ssh_file_read", map[string]any{
		"session_id": "big", "path": "/tmp/big.bin",
	}, &rd); err != nil {
		t.Fatalf("read big: %v", err)
	}
	if rd.Bytes != sz || len(rd.Data) != sz {
		t.Fatalf("size mismatch: wanted %d, got bytes=%d len(data)=%d", sz, rd.Bytes, len(rd.Data))
	}
}

// TestConnectDisconnectRace exercises the dial-vs-close race the post-fix audit
// caught. Under the old code, a Disconnect that fired between dialFinal
// succeeding and setClient leaked the freshly-dialed SSH client + jump
// clients + keepalive goroutine. The race is timing-dependent so we just
// make sure the daemon survives many overlapping pairs without deadlocking
// or panicking, then clean up. -race catches the underlying data races.
func TestConnectDisconnectRace(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	const iters = 12
	var wg sync.WaitGroup
	for i := 0; i < iters; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("rc%d", i)
			connectDone := make(chan struct{})
			go func() {
				_, _ = mc.Call(ctx, "ssh_connect", map[string]any{
					"id":   id,
					"spec": sshSpec(t, sshd.SSHPort),
				})
				close(connectDone)
			}()
			time.Sleep(time.Duration(i%5) * 5 * time.Millisecond)
			_, _ = mc.Call(ctx, "ssh_disconnect", map[string]any{"id": id})
			<-connectDone
			// Whichever order won, sweep up any leftover so ssh_list is clean.
			_, _ = mc.Call(ctx, "ssh_disconnect", map[string]any{"id": id})
		}()
	}
	wg.Wait()

	// Daemon should still be responsive and free of stale rc* sessions.
	var list []sshConnectResp
	if err := mc.CallJSON(ctx, "ssh_list", nil, &list); err != nil {
		t.Fatalf("ssh_list after race: %v", err)
	}
	for _, s := range list {
		if strings.HasPrefix(s.ID, "rc") {
			t.Errorf("session %s persisted past cleanup", s.ID)
		}
	}
}

// TestConcurrentShellWrites was a guaranteed race under the old code (shell
// Write was unsynchronized, so two goroutines into stdin would interleave
// bytes). The fix is a writeMu; with -race this would have screamed before.
func TestConcurrentShellWrites(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "c1", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	if _, err := mc.Call(ctx, "ssh_shell_open", map[string]any{"session_id": "c1", "shell_id": "sh"}); err != nil {
		t.Fatalf("shell_open: %v", err)
	}
	_, _ = mc.Call(ctx, "ssh_shell_read", map[string]any{"session_id": "c1", "shell_id": "sh", "timeout_ms": 500})

	var wg sync.WaitGroup
	const writers = 8
	wg.Add(writers)
	for i := 0; i < writers; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := mc.Call(ctx, "ssh_shell_write", map[string]any{
				"session_id": "c1", "shell_id": "sh",
				"data": fmt.Sprintf("echo writer-%d\n", i),
			})
			if err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	// Confirm we eventually see all 8 markers; lines may be ordered any way.
	var acc strings.Builder
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		var out struct {
			Data string `json:"data"`
		}
		_ = mc.CallJSON(ctx, "ssh_shell_read", map[string]any{
			"session_id": "c1", "shell_id": "sh", "timeout_ms": 400,
		}, &out)
		acc.WriteString(out.Data)
		if allPresent(acc.String(), writers) {
			break
		}
	}
	if !allPresent(acc.String(), writers) {
		t.Fatalf("not all writers' output observed:\n%s", acc.String())
	}
}

func allPresent(s string, n int) bool {
	for i := 0; i < n; i++ {
		if !strings.Contains(s, fmt.Sprintf("writer-%d", i)) {
			return false
		}
	}
	return true
}

// TestConcurrentReconnects exercises the reconnectMu serialization. Under the
// old code an explicit Reconnect concurrent with a keepalive-driven
// attemptReconnect would race, double-dial, and leak SSH clients.
func TestConcurrentReconnects(t *testing.T) {
	mc, sshd, _, teardown := setupSession(t)
	defer teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	spec := map[string]any{
		"user":           sshUser,
		"addresses":      []string{fmt.Sprintf("127.0.0.1:%d", sshd.SSHPort)},
		"auth":           keyAuth(t),
		"insecure":       true,
		"timeout":        "5s",
		"keepalive":      "1s",
		"auto_reconnect": true,
	}
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "rc", "spec": spec}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	var wg sync.WaitGroup
	const callers = 6
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			_, _ = mc.Call(ctx, "ssh_reconnect", map[string]any{"id": "rc"})
		}()
	}
	wg.Wait()

	// After the storm, session should be connected and exec should still work.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var info sshConnectResp
		if err := mc.CallJSON(ctx, "ssh_info", map[string]any{"id": "rc"}, &info); err == nil && info.State == "connected" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "rc", "command": "echo survived"}, &res); err != nil {
		t.Fatalf("exec after concurrent reconnects: %v", err)
	}
	if !strings.Contains(res.Stdout, "survived") {
		t.Fatalf("post-storm stdout %q", res.Stdout)
	}
}

// TestTOONFormat starts the daemon with -format=toon (which is also the
// production default) and confirms a tool result comes back as TOON text
// rather than JSON. We don't try to parse TOON — just verify it's
// shape-correct (an `items[N]{fields}:` header) and a few field tokens
// appear, since the goal is to catch a regression where TOON encoding
// silently falls back to JSON.
func TestTOONFormat(t *testing.T) {
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()
	daemon := StartDaemonWith(t, daemonBin, DaemonOpts{Format: "toon"})
	defer daemon.Stop()

	mc := NewMCPClientForDaemon(t, daemon)
	defer mc.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "tn", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	text, err := mc.CallText(ctx, "ssh_list", nil)
	if err != nil {
		t.Fatalf("ssh_list: %v", err)
	}
	// Negative check: JSON output would parse as JSON. TOON does not.
	var any any
	if err := json.Unmarshal([]byte(text), &any); err == nil {
		t.Fatalf("expected TOON output, got valid JSON:\n%s", text)
	}
	// TOON list output looks like `[1]{…}:` for a single-element array;
	// must contain the session id and state.
	if !strings.Contains(text, "tn") || !strings.Contains(text, "connected") {
		t.Fatalf("TOON output missing expected tokens:\n%s", text)
	}
	// And it should use the json-tagged field names (snake_case), not Go's
	// PascalCase, which would happen if json tags were ignored.
	if !strings.Contains(text, "state") || strings.Contains(text, "ActiveAddress") {
		t.Fatalf("TOON output not using json-tag field names:\n%s", text)
	}
}

func TestDaemonEnsureAutoSpawns(t *testing.T) {
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}

	stateDir := t.TempDir()
	handlePath := stateDir + "/daemon.json"

	// daemon.EnsureAt spawns the daemon itself when no live handle exists.
	// The daemon binds a free port (explicit -addr) and publishes {addr,pid} to
	// handlePath; EnsureAt reads it back. Pass state/lock/handle + a JSON
	// output pin as daemon args so this test's daemon is fully isolated.
	handle, err := daemon.EnsureAt(handlePath, daemonBin, []string{
		"-addr", "127.0.0.1:0",
		"-state", stateDir + "/state.json",
		"-lock", stateDir + "/daemon.lock",
		"-handle", handlePath,
		"-format", "json",
	})
	if err != nil {
		t.Fatalf("EnsureAt: %v", err)
	}
	t.Cleanup(func() {
		// The daemon was spawned detached. Read its pid and wait for it to
		// actually exit before TempDir cleanup removes files it has open.
		if pid, err := readPidFromLock(stateDir + "/daemon.lock"); err == nil {
			_ = exec.Command("kill", "-TERM", strconv.Itoa(pid)).Run()
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if err := exec.Command("kill", "-0", strconv.Itoa(pid)).Run(); err != nil {
					break
				}
				time.Sleep(80 * time.Millisecond)
			}
			_ = exec.Command("kill", "-KILL", strconv.Itoa(pid)).Run()
		}
	})

	d := &Daemon{addr: handle.Addr, handlePath: handlePath}
	mc := NewMCPClientForDaemon(t, d)
	defer mc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	var status struct {
		Uptime string `json:"uptime"`
	}
	if err := mc.CallJSON(ctx, "status", nil, &status); err != nil {
		t.Fatalf("status after auto-spawn: %v", err)
	}
	if status.Uptime == "" {
		t.Fatalf("daemon did not auto-spawn (empty uptime)")
	}
	t.Logf("auto-spawned daemon uptime=%s", status.Uptime)
}

func readPidFromLock(path string) (int, error) {
	data, err := exec.Command("cat", path).Output()
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func TestDaemonSurvivesClientReconnect(t *testing.T) {
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()
	daemon := StartDaemon(t, daemonBin)
	defer daemon.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	mc := NewMCPClientForDaemon(t, daemon)
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "persist1", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		mc.Close()
		t.Fatalf("connect: %v", err)
	}
	mc.Close()

	mc2 := NewMCPClientForDaemon(t, daemon)
	defer mc2.Close()
	var list []struct {
		ID    string `json:"id"`
		State string `json:"state"`
	}
	if err := mc2.CallJSON(ctx, "ssh_list", nil, &list); err != nil {
		t.Fatalf("ssh_list after restart: %v", err)
	}
	var found bool
	for _, s := range list {
		if s.ID == "persist1" && s.State == "connected" {
			found = true
			break
		}
	}
	if !found {
		raw, _ := json.Marshal(list)
		t.Fatalf("session did not survive client reconnect; ssh_list=%s", string(raw))
	}
}

// TestStreamableClientReconnectAcrossDaemonRestart keeps ONE MCP client
// object (one streamable-HTTP transport, one MCP session) alive across a
// full daemon restart — the scenario that is most likely to surprise us in
// the streamable transport, since the client caches a session id the new
// daemon never issued.
//
// What we verified while writing this test (worth knowing before touching
// transport config): the daemon's /mcp server uses mcp-go's default
// StatelessGeneratingSessionIdManager, which validates session-id *format*
// only, not existence. So a stale session id from the dead daemon is still
// accepted by the new one — re-running the initialize handshake on the same
// transport (Reinitialize) revives the client in place without ever seeing a
// 404/ErrSessionTerminated. That is exactly how a reconnecting remote MCP
// client recovers here.
func TestStreamableClientReconnectAcrossDaemonRestart(t *testing.T) {
	daemonBin := BuildDaemon(t)
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	EnsureDockerImage(t)
	sshd := StartSSHDContainer(t)
	defer sshd.Stop()

	// Both daemons share state/lock/handle paths so the restart looks like a
	// real one: same files, fresh process.
	stateDir := t.TempDir()
	opts := DaemonOpts{
		StatePath:  stateDir + "/state.json",
		LockPath:   stateDir + "/daemon.lock",
		HandlePath: stateDir + "/daemon.json",
	}

	daemon1 := StartDaemonWith(t, daemonBin, opts)
	addr := daemon1.Addr()

	// Client bound to the addr string, not the Daemon struct — it must
	// outlive the daemon it connected to.
	mc := NewMCPClientForAddr(t, addr)
	defer mc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "restart", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect on daemon1: %v", err)
	}
	var res execResp
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "restart", "command": "echo pre-restart"}, &res); err != nil {
		t.Fatalf("exec on daemon1: %v", err)
	}
	if !strings.Contains(res.Stdout, "pre-restart") {
		t.Fatalf("pre-restart stdout %q", res.Stdout)
	}
	sid1 := mc.SessionID()
	if sid1 == "" {
		t.Fatal("expected a negotiated MCP session id")
	}

	// Bounce the daemon in place: SIGTERM, then start a fresh process on the
	// same addr so the client's URL still points at the new server.
	daemon1.Stop()
	opts.Addr = addr
	daemon2 := StartDaemonWith(t, daemonBin, opts)
	defer daemon2.Stop()
	if daemon2.Addr() != addr {
		t.Fatalf("daemon2 bound to %s, want same addr %s", daemon2.Addr(), addr)
	}

	// The transport is untouched: same session id, same URL. A stale-session
	// request should still reach the server (session ids are stateless), but
	// the proper client-side recovery is to re-handshake — do that, and
	// confirm the client object is usable again.
	mc.Reinitialize()
	if sid2 := mc.SessionID(); sid2 == "" {
		t.Fatal("no session id after re-initialize")
	}

	// Non-persistent sessions don't survive a restart; reconnect and prove
	// the revived client can drive real work end to end.
	if _, err := mc.Call(ctx, "ssh_connect", map[string]any{"id": "restart2", "spec": sshSpec(t, sshd.SSHPort)}); err != nil {
		t.Fatalf("connect on daemon2 via reused client: %v", err)
	}
	if err := mc.CallJSON(ctx, "ssh_exec", map[string]any{"session_id": "restart2", "command": "echo post-restart"}, &res); err != nil {
		t.Fatalf("exec on daemon2 via reused client: %v", err)
	}
	if !strings.Contains(res.Stdout, "post-restart") {
		t.Fatalf("post-restart stdout %q", res.Stdout)
	}
}
