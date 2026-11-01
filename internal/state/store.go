package state

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

type Snapshot struct {
	Version     int                `json:"version"`
	SSHSessions []SSHSessionRecord `json:"ssh_sessions,omitempty"`
	// DisabledTools is the WebUI's per-tool on/off state, kept in the same
	// state file (no separate config). Restored on startup so a disabled
	// tool stays off across daemon restarts.
	DisabledTools []string `json:"disabled_tools,omitempty"`
}

type SSHSessionRecord struct {
	ID   string           `json:"id"`
	Spec sshx.ConnectSpec `json:"spec"`
}

type Store struct {
	mu   sync.Mutex
	path string
}

func DefaultPath() (string, error) {
	home, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "remote-shell-mcp", "state.json"), nil
}

func NewStore(path string) (*Store, error) {
	if path == "" {
		def, err := DefaultPath()
		if err != nil {
			return nil, err
		}
		path = def
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{path: path}, nil
}

func (s *Store) Path() string { return s.path }

// maxStateFileBytes guards Load() from trying to slurp a pathologically large
// state.json. The daemon writes this file itself; this only matters if it
// gets corrupted/replaced.
const maxStateFileBytes = 16 * 1024 * 1024

func (s *Store) Load() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Snapshot{Version: 1}, nil
		}
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() > maxStateFileBytes {
		return nil, fmt.Errorf("state file %s is %d bytes (>%d max)", s.path, st.Size(), maxStateFileBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxStateFileBytes {
		return nil, fmt.Errorf("state file %s exceeds %d bytes", s.path, maxStateFileBytes)
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("parse state file: %w", err)
	}
	if snap.Version == 0 {
		snap.Version = 1
	}
	return &snap, nil
}

func (s *Store) Save(snap *Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snap.Version == 0 {
		snap.Version = 1
	}
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		// Don't leave a half-written tmp file next to state.json.
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
