package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Handle is the rendezvous record the daemon writes after it has successfully
// bound a listener: addr is the actually-bound host:port (kernel-picked when
// the daemon was started with port 0), token is the per-run bearer token, pid
// is the daemon's PID so clients can target it for cleanup if the file
// is around but the process is wedged.
type Handle struct {
	Addr  string `json:"addr"`
	Token string `json:"token"`
	PID   int    `json:"pid"`
}

func WriteHandle(path string, h Handle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create handle dir: %w", err)
	}
	data, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func ReadHandle(path string) (Handle, error) {
	var h Handle
	data, err := os.ReadFile(path)
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, fmt.Errorf("parse handle %s: %w", path, err)
	}
	if h.Addr == "" || h.Token == "" {
		return h, fmt.Errorf("handle %s missing addr or token", path)
	}
	return h, nil
}
