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

// TestLockWritesPID — AcquireLock writes the daemon pid as plain text, which
// `ps` / shell-script cleanup depends on.
func TestLockWritesPID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "daemon.lock")

	l, err := AcquireLock(path)
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	defer l.Release()

	data, _ := os.ReadFile(path)
	if got, err := strconv.Atoi(strings.TrimSpace(string(data))); err != nil || got != os.Getpid() {
		t.Fatalf("lock pid = %q (parse err %v), want %d", string(data), err, os.Getpid())
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
