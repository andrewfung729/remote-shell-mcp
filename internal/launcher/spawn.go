package launcher

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jaenster/remote-shell-mcp/internal/daemon"
)

// daemonExeName is the daemon binary's filename for the same-directory
// probe in resolveDaemonBinary.
const daemonExeName = "remote-shell-mcpd"

// EnsureDaemon makes sure a daemon is running and returns the handle (addr +
// token) the caller should use to talk to it. If a handle on disk points at a
// daemon that's actually responsive, we reuse it. Otherwise — handle missing,
// daemon not listening, or any other half-broken state — we clean up the stale
// PID (if any), spawn a fresh daemon, and read the new handle it writes.
func EnsureDaemon(daemonBinary string, extraArgs []string) (daemon.Handle, error) {
	_, _, handlePath, err := daemon.DefaultPaths()
	if err != nil {
		return daemon.Handle{}, err
	}
	return EnsureDaemonAt(handlePath, daemonBinary, extraArgs)
}

// EnsureDaemonAt is EnsureDaemon with an explicit handle path — used by tests
// that isolate daemon state in a temp dir, and by power-users who want to run
// a non-default daemon location alongside the standard one. The corresponding
// lock path is derived (same directory, daemon.lock).
func EnsureDaemonAt(handlePath, daemonBinary string, extraArgs []string) (daemon.Handle, error) {
	lockPath := filepath.Join(filepath.Dir(handlePath), "daemon.lock")

	// Fast path: there's already a daemon and it's healthy.
	if h, ok := tryUseExistingDaemon(handlePath); ok {
		return h, nil
	}

	// Slow path: clean up stale state and spawn fresh.
	if err := killStaleDaemon(lockPath, handlePath); err != nil {
		// Best-effort cleanup; spawn will fail loudly if the port really is
		// still busy.
		_ = err
	}
	return spawnDaemon(daemonBinary, extraArgs, handlePath)
}

// tryUseExistingDaemon returns the on-disk handle if a daemon is responsive at
// the address it advertises. A non-listening addr means the daemon died (or
// the handle is leftover); the caller should clean up and respawn.
func tryUseExistingDaemon(handlePath string) (daemon.Handle, bool) {
	h, err := daemon.ReadHandle(handlePath)
	if err != nil {
		return daemon.Handle{}, false
	}
	if !daemon.IsListening(h.Addr, 200*time.Millisecond) {
		return daemon.Handle{}, false
	}
	return h, true
}

// killStaleDaemon is best-effort: if daemon.lock has a still-alive PID, send
// it SIGTERM and wait for it to release the port + lock. After a short grace
// period we escalate to SIGKILL — we'd rather leave the user with a fresh
// daemon than have the launcher stuck forever because some wedged old process
// is squatting on the port.
func killStaleDaemon(lockPath, handlePath string) error {
	// Remove leftover handle first so a future fast-path won't pick up stale
	// addr/token while the new daemon is still coming up.
	_ = os.Remove(handlePath)

	pid, err := daemon.ReadPid(lockPath)
	if err != nil || pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// Send graceful shutdown signal; ignore any error (process gone, EPERM,
	// etc. — we just want it dead).
	_ = proc.Signal(os.Interrupt)
	if waitForExit(proc, 2*time.Second) {
		return nil
	}
	// Stubborn process — escalate to SIGKILL. We'd rather leave the user
	// with a fresh daemon than hang forever on a wedged one.
	_ = proc.Kill()
	_ = waitForExit(proc, 2*time.Second)
	return nil
}

// waitForExit returns true once Signal(0) reports the process is gone.
func waitForExit(proc *os.Process, total time.Duration) bool {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true // gone
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func spawnDaemon(daemonBinary string, extraArgs []string, handlePath string) (daemon.Handle, error) {
	bin, err := resolveDaemonBinary(daemonBinary)
	if err != nil {
		return daemon.Handle{}, err
	}
	cmd := exec.Command(bin, extraArgs...)
	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return daemon.Handle{}, fmt.Errorf("spawn daemon %s: %w", bin, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return daemon.Handle{}, fmt.Errorf("release daemon process: %w", err)
	}
	// Wait for the daemon to publish its handle (it writes the file AFTER it
	// has bound the listener, so a readable handle means the addr is live).
	return waitForHandle(handlePath, defaultStartTimeout)
}

func waitForHandle(path string, total time.Duration) (daemon.Handle, error) {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		if h, err := daemon.ReadHandle(path); err == nil {
			if daemon.IsListening(h.Addr, 200*time.Millisecond) {
				return h, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return daemon.Handle{}, fmt.Errorf("daemon handle %s never appeared (daemon may have failed to start)", path)
}

func resolveDaemonBinary(override string) (string, error) {
	if override != "" {
		if filepath.IsAbs(override) {
			if _, err := os.Stat(override); err != nil {
				return "", err
			}
			return override, nil
		}
		if p, err := exec.LookPath(override); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("remote-shell-mcpd"); err == nil {
		return p, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	dir := filepath.Dir(exe)
	candidate := filepath.Join(dir, daemonExeName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	base := strings.TrimSuffix(filepath.Base(exe), filepath.Ext(exe))
	if base == "remote-shell-mcp" {
		candidate = filepath.Join(dir, daemonExeName)
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", errors.New("could not locate remote-shell-mcpd binary; set --daemon-binary or put it on PATH")
}
