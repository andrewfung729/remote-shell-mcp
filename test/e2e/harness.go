//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	containerLabel = "remote-shell-mcp-e2e=1"
	sshUser        = "root"
)

// sshdImageTag names a fixture image by the content of its Dockerfile, so a
// changed fixture rebuilds instead of silently reusing an image cached from an
// older revision. Two variants exist: the default (bash installed — the shell
// layer's primary path) and the "-nobash" one that pins the degraded fallback.
func sshdImageTag(t *testing.T, withBash bool) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(RepoRoot(t), "test", "e2e", "Dockerfile.sshd"))
	if err != nil {
		t.Fatalf("read Dockerfile.sshd: %v", err)
	}
	sum := sha256.Sum256(data)
	tag := "remote-shell-mcp-test-sshd:" + hex.EncodeToString(sum[:4])
	if !withBash {
		tag += "-nobash"
	}
	return tag
}

func RepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not locate harness file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

var (
	buildOnce sync.Once
	buildErr  error
	builtBin  string
)

// BuildDaemon builds the remote-shell-mcpd binary once for the suite. The
// daemon is the only binary — there is no stdio launcher to build.
func BuildDaemon(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		root := RepoRoot(t)
		builtBin = filepath.Join(root, "bin", "remote-shell-mcpd")
		// Always rebuild: Go's build cache makes incremental rebuilds cheap,
		// and a stale binary masks source changes.
		cmd := exec.Command("go", "build", "-o", "bin/remote-shell-mcpd", "./cmd/remote-shell-mcpd")
		cmd.Dir = root
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			buildErr = fmt.Errorf("build remote-shell-mcpd: %w", err)
		}
	})
	if buildErr != nil {
		t.Fatalf("%v", buildErr)
	}
	return builtBin
}

var (
	imageOnce sync.Once
	imageErr  error
)

// EnsureDockerImage builds both sshd fixtures once per run. The once matters
// under t.Parallel: without it two tests that start together both miss the
// inspect check and race docker build on the same tag.
func EnsureDockerImage(t *testing.T) {
	t.Helper()
	imageOnce.Do(func() {
		for _, withBash := range []bool{true, false} {
			if err := buildSSHDImage(t, withBash); err != nil {
				imageErr = err
				return
			}
		}
	})
	if imageErr != nil {
		t.Fatalf("%v", imageErr)
	}
}

func buildSSHDImage(t *testing.T, withBash bool) error {
	t.Helper()
	tag := sshdImageTag(t, withBash)
	// Exit code is authoritative; the previous "string contains Error" check
	// false-matched on legitimate JSON whose labels/env happened to contain
	// the substring.
	if err := exec.Command("docker", "image", "inspect", tag).Run(); err == nil {
		return nil
	}
	root := RepoRoot(t)
	ctx := filepath.Join(root, "test", "e2e")
	withBashArg := "0"
	if withBash {
		withBashArg = "1"
	}
	cmd := exec.Command("docker", "build",
		"--build-arg", "WITH_BASH="+withBashArg,
		"-t", tag,
		"-f", filepath.Join(ctx, "Dockerfile.sshd"), ctx)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build %s: %w", tag, err)
	}
	return nil
}

var (
	suiteKeyOnce sync.Once
	suiteKeyErr  error
	suiteKeyPriv string // private key path
	suiteKeyPub  string // authorized_keys line
)

// SuiteKey returns the suite-wide ephemeral ed25519 keypair. Generated once
// per `go test` invocation into a process-level temp dir (t.TempDir would be
// torn down by the first caller, but the same key is shared by every test).
// The private key path goes into ssh_connect specs as auth.key_path; the
// public half is installed into each test container by StartSSHDContainer
// (or ad-hoc via InstallAuthorizedKey).
func SuiteKey(t *testing.T) (privPath, pubAuthorized string) {
	t.Helper()
	suiteKeyOnce.Do(func() {
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			suiteKeyErr = fmt.Errorf("ed25519: %w", err)
			return
		}
		sshPub, err := ssh.NewPublicKey(pub)
		if err != nil {
			suiteKeyErr = fmt.Errorf("ssh pub: %w", err)
			return
		}
		suiteKeyPub = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
		pemBlock, err := ssh.MarshalPrivateKey(priv, "")
		if err != nil {
			suiteKeyErr = fmt.Errorf("marshal priv: %w", err)
			return
		}
		dir, err := os.MkdirTemp("", "remote-shell-mcp-e2e-key-")
		if err != nil {
			suiteKeyErr = fmt.Errorf("mktemp for suite key: %w", err)
			return
		}
		suiteKeyPriv = filepath.Join(dir, "id_ed25519")
		if err := os.WriteFile(suiteKeyPriv, pem.EncodeToMemory(pemBlock), 0o600); err != nil {
			suiteKeyErr = fmt.Errorf("write priv: %w", err)
		}
	})
	if suiteKeyErr != nil {
		t.Fatalf("suite key: %v", suiteKeyErr)
	}
	return suiteKeyPriv, suiteKeyPub
}

// keyIdentityFile returns the IdentityFile line for the suite key — the
// same role the old spec.auth.key_path played, now delivered via ssh_config.
func keyIdentityFile(t *testing.T) string {
	t.Helper()
	priv, _ := SuiteKey(t)
	return priv
}

// testHome builds a fake $HOME for the daemon: ~/.ssh/config holding one
// concrete Host entry per alias, plus ~/.ssh/known_hosts seeded with the
// container's host key — host-key verification is always strict (no
// insecure mode), so the suite must pre-populate known_hosts the way a
// human would by ssh'ing once. sshd's public key is fetched from the
// container over docker exec (the /etc/ssh/*.pub files the image's
// ssh-keygen -A wrote).
func testHome(t *testing.T, blocks ...string) string {
	t.Helper()
	dir := t.TempDir()
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatalf("mkdir .ssh: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(strings.Join(blocks, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write ssh config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "known_hosts"), nil, 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return dir
}

// testHomeForContainer is testHome + the container's sshd host key written
// into known_hosts under "[127.0.0.1]:<port>" — the strict-check fixture.
func testHomeForContainer(t *testing.T, sshd *SSHDContainer, blocks ...string) string {
	t.Helper()
	dir := testHome(t, blocks...)
	pub := sshd.Exec(t, "cat", "/etc/ssh/ssh_host_ed25519_key.pub")
	line := fmt.Sprintf("[127.0.0.1]:%d %s", sshd.SSHPort, strings.TrimSpace(pub))
	kh := filepath.Join(dir, ".ssh", "known_hosts")
	if err := os.WriteFile(kh, []byte(line+"\n"), 0o600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	return dir
}

// hostBlock writes one concrete Host entry for alias pointing at
// 127.0.0.1:port with the suite key as IdentityFile.
func hostBlock(t *testing.T, alias string, port int) string {
	t.Helper()
	return fmt.Sprintf("Host %s\n  HostName 127.0.0.1\n  Port %d\n  User %s\n  IdentityFile %s",
		alias, port, sshUser, keyIdentityFile(t))
}

// SweepStaleContainers removes any leftover containers from previous failed
// runs of this suite.
//
// It must only be called before the suite starts parallel tests: it removes
// EVERY container carrying the label, which under t.Parallel would kill the
// containers of tests running alongside the caller. Nothing calls it today —
// keep it that way, or call it from a package-level TestMain before m.Run.
func SweepStaleContainers() {
	for _, label := range []string{containerLabel, "remote-shell-mcp-e2e-lifecycle=1"} {
		ids, err := exec.Command("docker", "ps", "-aq", "--filter", "label="+label).Output()
		if err != nil {
			continue
		}
		for _, id := range strings.Fields(string(ids)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
}

type SSHDContainer struct {
	ID      string
	SSHPort int
	t       *testing.T
}

// StartSSHDContainer launches the test sshd container and installs the
// ephemeral suite key into /root/.ssh/authorized_keys. The key lives in the
// container, not the image: the suite key regenerates per `go test` run,
// while the image is cached across runs — baking it in would pin the image
// to a key that no longer exists on the next run.
func StartSSHDContainer(t *testing.T) *SSHDContainer {
	t.Helper()
	return startSSHDContainer(t, true)
}

// StartSSHDContainerNoBash starts the fixture a minimal host looks like: no
// bash, so the shell layer has to fall back to a POSIX sh.
func StartSSHDContainerNoBash(t *testing.T) *SSHDContainer {
	t.Helper()
	return startSSHDContainer(t, false)
}

func startSSHDContainer(t *testing.T, withBash bool) *SSHDContainer {
	t.Helper()
	_, pub := SuiteKey(t)
	args := []string{"run", "-d", "--rm",
		"--label", containerLabel,
		"-p", "127.0.0.1::22",
		sshdImageTag(t, withBash),
	}
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker run: %v", err)
	}
	id := strings.TrimSpace(string(out))
	c := &SSHDContainer{ID: id, t: t}
	port, err := waitForPort(id)
	if err != nil {
		c.Stop()
		t.Fatalf("get sshd port: %v", err)
	}
	c.SSHPort = port
	InstallAuthorizedKey(t, c, pub)
	if err := waitForSSH(port); err != nil {
		c.Stop()
		t.Fatalf("wait for sshd: %v", err)
	}
	return c
}

func waitForPort(id string) (int, error) {
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("docker", "port", id, "22/tcp").Output()
		if err == nil {
			line := strings.TrimSpace(string(out))
			if idx := strings.LastIndex(line, ":"); idx >= 0 {
				if p, err := strconv.Atoi(strings.TrimSpace(line[idx+1:])); err == nil {
					return p, nil
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return 0, fmt.Errorf("no port mapping for container")
}

func waitForSSH(port int) error {
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
		if err == nil {
			_ = c.Close()
			time.Sleep(200 * time.Millisecond)
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("sshd never came up on 127.0.0.1:%d", port)
}

func (c *SSHDContainer) Stop() {
	if c == nil || c.ID == "" {
		return
	}
	_ = exec.Command("docker", "kill", c.ID).Run()
}

func (c *SSHDContainer) Exec(t *testing.T, cmd ...string) string {
	t.Helper()
	args := append([]string{"exec", c.ID}, cmd...)
	out, err := exec.Command("docker", args...).Output()
	if err != nil {
		t.Fatalf("docker exec %v: %v", cmd, err)
	}
	return string(out)
}

func (c *SSHDContainer) ExecAsync(cmd ...string) {
	args := append([]string{"exec", c.ID}, cmd...)
	go func() {
		_ = exec.Command("docker", args...).Run()
	}()
}

func PickFreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port
}

type Daemon struct {
	cmd        *exec.Cmd
	addr       string
	handlePath string
	statePath  string
	stop       context.CancelFunc
	stderr     *syncBuffer
	stdout     *syncBuffer // captures the printed WebUI token
	token      string      // one-time WebUI token scraped from stdout
	authCookie string      // session cookie after a successful exchange
}

type DaemonOpts struct {
	Addr       string // override bind addr; empty = pick a free loopback port
	StatePath  string
	LockPath   string
	HandlePath string
	Env        []string
}

func StartDaemon(t *testing.T, daemonBin string) *Daemon {
	t.Helper()
	return StartDaemonWith(t, daemonBin, DaemonOpts{})
}

// StartDaemonWith starts a daemon and waits for its HTTP surface.
//
// With no explicit Addr the port is picked from a free one and only then
// bound — a race two parallel tests can lose: the winner's daemon takes the
// port and the loser's exits non-zero. Retry on a fresh port instead of
// failing the test; an explicit Addr is the caller's own choice, not retried.
func StartDaemonWith(t *testing.T, daemonBin string, opts DaemonOpts) *Daemon {
	t.Helper()
	attempts := 5
	if opts.Addr != "" {
		attempts = 1
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		d, err := startDaemonOnce(t, daemonBin, opts)
		if err == nil {
			return d
		}
		lastErr = err
	}
	t.Fatalf("start daemon: %v", lastErr)
	return nil
}

func startDaemonOnce(t *testing.T, daemonBin string, opts DaemonOpts) (*Daemon, error) {
	t.Helper()
	addr := opts.Addr
	if addr == "" {
		addr = fmt.Sprintf("127.0.0.1:%d", PickFreePort(t))
	}
	dir := t.TempDir()
	if opts.StatePath == "" {
		opts.StatePath = filepath.Join(dir, "state.json")
	}
	if opts.LockPath == "" {
		opts.LockPath = filepath.Join(dir, "daemon.lock")
	}
	if opts.HandlePath == "" {
		opts.HandlePath = filepath.Join(dir, "daemon.json")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, daemonBin,
		"-addr", addr,
		"-state", opts.StatePath,
		"-lock", opts.LockPath,
		"-handle", opts.HandlePath,
		"-log", "text",
	)
	cmd.Env = append(os.Environ(), opts.Env...)
	var stderr, stdout syncBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, err
	}
	d := &Daemon{cmd: cmd, addr: addr, handlePath: opts.HandlePath, statePath: opts.StatePath, stop: cancel, stderr: &stderr, stdout: &stdout}
	if err := waitForHTTP(addr); err != nil {
		d.Stop()
		return nil, fmt.Errorf("%s never came up: %w; stderr:\n%s", addr, err, stderr.String())
	}
	d.token = scrapeToken(stdout.String())
	return d, nil
}

// syncBuffer is a bytes.Buffer safe for concurrent use. exec.Cmd writes to
// stdout/stderr from its own copy goroutine while the test reads them (token
// scrape, stderr diagnostics) — a bare bytes.Buffer races there.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (d *Daemon) HandlePath() string { return d.handlePath }

func (d *Daemon) Addr() string { return d.addr }

// scrapeToken pulls the printed WebUI token out of the daemon's stdout —
// the token the operator sees in their terminal.
func scrapeToken(out string) string {
	for _, line := range strings.FieldsFunc(out, func(r rune) bool { return r == 10 }) {
		if i := strings.Index(line, "one-time token: "); i >= 0 {
			return strings.TrimSpace(line[i+len("one-time token: "):])
		}
	}
	return ""
}

func (d *Daemon) StatePath() string { return d.statePath }

// Token returns the one-time WebUI token scraped from daemon stdout ("" if
// the daemon printed none — e.g. a pre-token build).
func (d *Daemon) Token() string { return d.token }

func (d *Daemon) Stop() {
	if d == nil || d.cmd == nil || d.cmd.Process == nil {
		return
	}
	// SIGTERM gives the daemon a chance to flush state and shut down its
	// transport sessions. Escalate to SIGKILL via ctx cancel if it ignores us.
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	graceful := make(chan struct{})
	go func() { _ = d.cmd.Wait(); close(graceful) }()
	select {
	case <-graceful:
	case <-time.After(3 * time.Second):
		if d.stop != nil {
			d.stop()
		}
		<-graceful
	}
}

// GenerateKey creates an ed25519 keypair on disk. Returns the private key path
// (OpenSSH PEM format) and the public key in authorized_keys form (single line).
func GenerateKey(t *testing.T) (privPath, pubAuthorized string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("ssh pub: %v", err)
	}
	pubAuthorized = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))

	pemBlock, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal priv: %v", err)
	}
	privBytes := pem.EncodeToMemory(pemBlock)
	dir := t.TempDir()
	privPath = filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(privPath, privBytes, 0o600); err != nil {
		t.Fatalf("write priv: %v", err)
	}
	return
}

// InstallAuthorizedKey appends an authorized_keys entry inside the container.
func InstallAuthorizedKey(t *testing.T, sshd *SSHDContainer, pub string) {
	t.Helper()
	cmd := exec.Command("docker", "exec", "-i", sshd.ID, "sh", "-c",
		"mkdir -p /root/.ssh && chmod 700 /root/.ssh && cat >> /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys")
	cmd.Stdin = strings.NewReader(pub + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("install authorized_keys: %v\n%s", err, string(out))
	}
}

// StartSSHAgent spawns an ssh-agent, adds the given key, and returns the socket
// path. The agent is killed at test cleanup.
func StartSSHAgent(t *testing.T, keyPath string) string {
	t.Helper()
	out, err := exec.Command("ssh-agent", "-c").Output()
	if err != nil {
		t.Skipf("ssh-agent not available: %v", err)
	}
	var sock, pidStr string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "setenv SSH_AUTH_SOCK ") {
			sock = strings.TrimSuffix(strings.TrimPrefix(line, "setenv SSH_AUTH_SOCK "), ";")
		}
		if strings.HasPrefix(line, "setenv SSH_AGENT_PID ") {
			pidStr = strings.TrimSuffix(strings.TrimPrefix(line, "setenv SSH_AGENT_PID "), ";")
		}
	}
	if sock == "" {
		t.Fatalf("could not parse ssh-agent output: %s", string(out))
	}
	add := exec.Command("ssh-add", keyPath)
	add.Env = append(os.Environ(), "SSH_AUTH_SOCK="+sock)
	if out, err := add.CombinedOutput(); err != nil {
		t.Fatalf("ssh-add: %v\n%s", err, string(out))
	}
	t.Cleanup(func() {
		if pidStr != "" {
			_ = exec.Command("kill", pidStr).Run()
		}
	})
	return sock
}

func waitForHTTP(addr string) error {
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("address %s never accepted connections", addr)
}
