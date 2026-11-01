package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestLockExclusive verifies the platform-specific lock primitive: a second
// AcquireLock on the same path while the first is still held must fail. This
// is what prevents two daemons binding the same TCP port.
func TestLockExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.lock")

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	defer first.Release()

	second, err := AcquireLock(path)
	if err == nil {
		second.Release()
		t.Fatalf("second AcquireLock should have failed while first is held")
	}
	if !strings.Contains(err.Error(), "daemon already running") {
		t.Fatalf("expected 'daemon already running' in error, got: %v", err)
	}
}

// TestLockReleaseAllowsReacquire — releasing the lock must let another caller
// take it. Without this we'd accumulate "stuck" lock files after every
// daemon restart.
func TestLockReleaseAllowsReacquire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.lock")

	first, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}
	first.Release()

	second, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("re-acquire after release: %v", err)
	}
	defer second.Release()
}

// TestLockWritesPID — ReadPid must return whatever AcquireLock wrote, so the
// clients can show "daemon running as pid N" diagnostics.
func TestLockWritesPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.lock")

	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer l.Release()

	pid, err := ReadPid(path)
	if err != nil {
		t.Fatalf("ReadPid: %v", err)
	}
	if pid != os.Getpid() {
		t.Fatalf("ReadPid=%d, want %d", pid, os.Getpid())
	}
	// Also check the file contents directly so we know it's plain text
	// (we depend on this for `ps` / shell scripts).
	data, _ := os.ReadFile(path)
	if _, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil {
		t.Fatalf("lock file contents not numeric: %q", data)
	}
}

// TestLockReleaseIsIdempotent — Release on a nil or already-released lock is
// a no-op, which we rely on in defer chains where the lock may have been
// released earlier on the happy path.
func TestLockReleaseIsIdempotent(t *testing.T) {
	var nilLock *Lock
	nilLock.Release() // must not panic

	dir := t.TempDir()
	l, err := AcquireLock(filepath.Join(dir, "lock"))
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	l.Release()
	l.Release() // second call must not panic or error
}
