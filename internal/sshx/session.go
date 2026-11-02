package sshx

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

type SessionState string

const (
	StateConnecting   SessionState = "connecting"
	StateConnected    SessionState = "connected"
	StateReconnecting SessionState = "reconnecting"
	StateDisconnected SessionState = "disconnected"
)

type Session struct {
	ID   string
	Spec ConnectSpec

	mu          sync.RWMutex
	client      *ssh.Client
	jumpClients []*ssh.Client
	activeAddr  string
	state       SessionState
	lastError   string
	connectedAt time.Time

	shellsMu sync.Mutex
	shells   map[string]*Shell // keyed by shell name

	sftpMu sync.Mutex
	sftp   *sftp.Client

	reconnectMu sync.Mutex // serializes attemptReconnect and Reconnect

	keepaliveCancel context.CancelFunc
	closed          atomic.Bool
}

func (s *Session) getClient() (*ssh.Client, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return nil, fmt.Errorf("session %q is %s", s.ID, s.state)
	}
	return s.client, nil
}

// setClient installs the dialed client onto the session. If the session was
// concurrently closed (the caller of Connect or Reconnect lost a race against
// Disconnect) the new client is closed immediately so it isn't leaked.
// Returns true if the client was installed, false if the session was closed.
func (s *Session) setClient(c *ssh.Client, jumps []*ssh.Client, addr string) bool {
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		_ = c.Close()
		for _, jc := range jumps {
			_ = jc.Close()
		}
		return false
	}
	s.client = c
	s.jumpClients = jumps
	s.activeAddr = addr
	s.state = StateConnected
	s.lastError = ""
	s.connectedAt = time.Now()
	s.mu.Unlock()
	return true
}

func (s *Session) markState(state SessionState, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state
	if err != nil {
		s.lastError = err.Error()
	}
}

// clearClient atomically detaches the SSH transport AND the SFTP client that
// rides on it. Holding both mutexes during the swap closes a window where
// sftpClient() could observe sftp=nil with the OLD ssh.Client still set, and
// build a new SFTP on a transport that is about to be closed.
func (s *Session) clearClient() {
	s.sftpMu.Lock()
	s.mu.Lock()
	sft := s.sftp
	cli := s.client
	jumps := s.jumpClients
	s.sftp = nil
	s.client = nil
	s.jumpClients = nil
	s.mu.Unlock()
	s.sftpMu.Unlock()

	if sft != nil {
		_ = sft.Close()
	}
	if cli != nil {
		_ = cli.Close()
	}
	for _, jc := range jumps {
		_ = jc.Close()
	}
}

func (s *Session) close() {
	if !s.closed.CompareAndSwap(false, true) {
		return
	}
	// Grab and clear keepaliveCancel under s.mu so we don't race with a
	// concurrent startKeepalive setting it.
	s.mu.Lock()
	cancel := s.keepaliveCancel
	s.keepaliveCancel = nil
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	// Drain the shells map but keep it non-nil so any racing OpenShell
	// fails the closed check below cleanly rather than panicking on a nil map.
	s.shellsMu.Lock()
	for id, sh := range s.shells {
		_ = sh.Close()
		delete(s.shells, id)
	}
	s.shellsMu.Unlock()
	// clearClient already atomically drops both the SSH client and the SFTP
	// client that rides on it — no need to call closeSFTP separately.
	s.clearClient()
	s.markState(StateDisconnected, nil)
}

func (s *Session) Info() SessionInfo {
	s.mu.RLock()
	state, lastErr, addr, connAt := s.state, s.lastError, s.activeAddr, s.connectedAt
	s.mu.RUnlock()
	s.shellsMu.Lock()
	shells := make([]string, 0, len(s.shells))
	for id := range s.shells {
		shells = append(shells, id)
	}
	s.shellsMu.Unlock()
	return SessionInfo{
		ID:            s.ID,
		User:          s.Spec.User,
		Addresses:     s.Spec.Addresses,
		ActiveAddress: addr,
		State:         string(state),
		LastError:     lastErr,
		ConnectedAt:   connAt,
		Shells:        shells,
	}
}

type SessionInfo struct {
	ID            string    `json:"id"`
	User          string    `json:"user"`
	Addresses     []string  `json:"addresses"`
	ActiveAddress string    `json:"active_address,omitempty"`
	State         string    `json:"state"`
	LastError     string    `json:"last_error,omitempty"`
	ConnectedAt   time.Time `json:"connected_at,omitempty"`
	Shells        []string  `json:"shells"`
}
