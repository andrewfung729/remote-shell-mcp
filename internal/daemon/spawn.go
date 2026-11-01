package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// exeName is the daemon binary's filename for the same-directory probe in
// resolveBinary.
const exeName = "remote-shell-mcpd"

// Ensure makes sure a daemon is running and returns the handle (addr + token)
// a caller should use to talk to it. If a handle on disk points at a daemon
// that's actually responsive, we reuse it. Otherwise — handle missing, daemon
// not listening, or any other half-broken state — we clean up the stale PID
// (if any), spawn a fresh daemon, and read the new handle it writes.
//
// This lives in the daemon package (not a separate launcher) because the
// daemon is now the only binary: there is no stdio proxy. Whoever wants a
// daemon — a remote-MCP client registration, the future Mac app, a CLI —
// calls this to bring one up on demand.
func Ensure(daemonBinary string, extraArgs []string) (Handle, error) {
	_, _, handlePath, err := DefaultPaths()
	if err != nil {
		return Handle{}, err
	}
	return EnsureAt(handlePath, daemonBinary, extraArgs)
}

// EnsureAt is Ensure with an explicit handle path — used by tests that
// isolate daemon state in a temp dir, and by power-users who want a
// non-default daemon location alongside the standard one. The corresponding
// lock path is derived (same directory, daemon.lock).
func EnsureAt(handlePath, daemonBinary string, extraArgs []string) (Handle, error) {
	lockPath := filepath.Join(filepath.Dir(handlePath), "daemon.lock")

	// Fast path: there's already a daemon and it's healthy.
	if h, ok := tryUseExisting(handlePath); ok {
		return h, nil
	}

	// Slow path: clean up stale state and spawn fresh.
	_ = killStale(lockPath, handlePath)
	return spawn(daemonBinary, extraArgs, handlePath)
}

// tryUseExisting returns the on-disk handle if a daemon is responsive at the
// address it advertises. A non-listening addr means the daemon died (or the
// handle is leftover); the caller should clean up and respawn.
func tryUseExisting(handlePath string) (Handle, bool) {
	h, err := ReadHandle(handlePath)
	if err != nil {
		return Handle{}, false
	}
	if !IsListening(h.Addr, 200*time.Millisecond) {
		return Handle{}, false
	}
	return h, true
}

// killStale is best-effort: if daemon.lock has a still-alive PID, send it
// SIGTERM and wait for it to release the port + lock. After a short grace
// period we escalate to SIGKILL — we'd rather leave the user with a fresh
// daemon than have the caller stuck forever because a wedged old process is
// squatting on the port.
func killStale(lockPath, handlePath string) error {
	// Remove leftover handle first so a future fast-path won't pick up stale
	// addr/token while the new daemon is still coming up.
	_ = os.Remove(handlePath)

	pid, err := ReadPid(lockPath)
	if err != nil || pid <= 0 {
		return nil
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	_ = proc.Signal(os.Interrupt)
	if waitForExit(proc, 2*time.Second) {
		return nil
	}
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

func spawn(daemonBinary string, extraArgs []string, handlePath string) (Handle, error) {
	bin, err := resolveBinary(daemonBinary)
	if err != nil {
		return Handle{}, err
	}
	cmd := exec.Command(bin, extraArgs...)
	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	detachDaemon(cmd)
	if err := cmd.Start(); err != nil {
		return Handle{}, fmt.Errorf("spawn daemon %s: %w", bin, err)
	}
	if err := cmd.Process.Release(); err != nil {
		return Handle{}, fmt.Errorf("release daemon process: %w", err)
	}
	// Wait for the daemon to publish its handle (it writes the file AFTER it
	// has bound the listener, so a readable handle means the addr is live).
	return waitForHandle(handlePath, defaultStartTimeout)
}

func waitForHandle(path string, total time.Duration) (Handle, error) {
	deadline := time.Now().Add(total)
	for time.Now().Before(deadline) {
		if h, err := ReadHandle(path); err == nil {
			if IsListening(h.Addr, 200*time.Millisecond) {
				return h, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return Handle{}, fmt.Errorf("daemon handle %s never appeared (daemon may have failed to start)", path)
}

func resolveBinary(override string) (string, error) {
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
	candidate := filepath.Join(filepath.Dir(exe), exeName)
	if _, err := os.Stat(candidate); err == nil {
		return candidate, nil
	}
	return "", errors.New("could not locate remote-shell-mcpd binary; pass an explicit path or put it on PATH")
}
