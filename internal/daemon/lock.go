package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

type Lock struct {
	path string
	file *os.File
}

// AcquireLock takes an exclusive, non-blocking lock on path via flock(2) —
// the locking primitive lives in lock_impl.go. already-held → error.
func AcquireLock(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := acquireLockHandle(f); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("daemon already running (lock held on %s)", path)
	}
	if err := f.Truncate(0); err != nil {
		_ = f.Close()
		return nil, err
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid())); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{path: path, file: f}, nil
}

func (l *Lock) Release() {
	if l == nil || l.file == nil {
		return
	}
	_ = releaseLockHandle(l.file)
	_ = l.file.Close()
	_ = os.Remove(l.path)
}

func DefaultPaths() (lockPath, statePath, handlePath string, err error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", "", "", err
	}
	dir := filepath.Join(cfg, "remote-shell-mcp")
	return filepath.Join(dir, "daemon.lock"),
		filepath.Join(dir, "state.json"),
		filepath.Join(dir, "daemon.json"), nil
}
