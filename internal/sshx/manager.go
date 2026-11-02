package sshx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewManager() *Manager {
	return &Manager{sessions: map[string]*Session{}}
}

// ErrNoSSHConfigEntry is returned when the host alias matches no concrete
// Host in the daemon user's ~/.ssh/config. Connection details and auth
// resolve only from ssh_config — a wildcard or missing entry can't be used.
var ErrNoSSHConfigEntry = errNoSSHConfigEntry

// Connect dials (or returns the existing) session for host. The session id
// is the host alias itself — one session per host, created transparently
// on first use. Everything below the alias — connection details and auth —
// resolves from the daemon user's ~/.ssh/config, like the `ssh` CLI.
// Keepalive (30s) and auto-reconnect are always on; host-key verification
// is always strict against ~/.ssh/known_hosts.
func (m *Manager) Connect(host string) (*Session, error) {
	if host == "" {
		return nil, errors.New("host is required")
	}
	m.mu.RLock()
	if existing, ok := m.sessions[host]; ok {
		m.mu.RUnlock()
		return existing, nil
	}
	m.mu.RUnlock()

	if !hasConfigEntry(host) {
		return nil, errNoSSHConfigEntry
	}
	spec := ConnectSpec{Addresses: []string{host}}
	resolveFromSSHConfig(&spec)
	if spec.User == "" {
		return nil, errors.New("no user: the ssh_config entry doesn't set User (add one to ~/.ssh/config)")
	}
	for _, j := range spec.JumpHosts {
		if j.User == "" {
			return nil, errors.New("a jump host has no user: set User in its ssh_config entry")
		}
	}
	id := host

	m.mu.Lock()
	if existing, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return existing, nil
	}
	sess := &Session{
		ID:     id,
		Spec:   spec,
		state:  StateConnecting,
		shells: map[string]*Shell{},
	}
	m.sessions[id] = sess
	m.mu.Unlock()

	client, jumps, addr, err := dialFinal(spec)
	if err != nil {
		m.mu.Lock()
		delete(m.sessions, id)
		m.mu.Unlock()
		return nil, err
	}
	if !sess.setClient(client, jumps, addr) {
		// Session was disconnected mid-dial. setClient already closed the
		// new connection — clean up the map entry if it's still there.
		m.mu.Lock()
		if existing, ok := m.sessions[id]; ok && existing == sess {
			delete(m.sessions, id)
		}
		m.mu.Unlock()
		return nil, errors.New("session was disconnected during connect")
	}

	sess.startKeepalive()
	return sess, nil
}

func (m *Manager) Get(id string) (*Session, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[id]
	if !ok {
		return nil, fmt.Errorf("ssh session %q not found", id)
	}
	return s, nil
}

func (m *Manager) Disconnect(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("ssh session %q not found", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	s.close()
	return nil
}

func (m *Manager) Reconnect(id string) (*Session, error) {
	s, err := m.Get(id)
	if err != nil {
		return nil, err
	}
	s.reconnectMu.Lock()
	defer s.reconnectMu.Unlock()

	s.clearClient()
	s.markState(StateReconnecting, nil)

	client, jumps, addr, err := dialFinal(s.Spec)
	if err != nil {
		s.markState(StateDisconnected, err)
		return nil, err
	}
	if !s.setClient(client, jumps, addr) {
		return nil, errors.New("session was closed during reconnect")
	}
	return s, nil
}

func (m *Manager) List() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s.Info())
	}
	return out
}

func (m *Manager) Sessions() map[string]*Session {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]*Session, len(m.sessions))
	for k, v := range m.sessions {
		out[k] = v
	}
	return out
}

func (m *Manager) CloseAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, s := range m.sessions {
		s.close()
		delete(m.sessions, id)
	}
}

// keepaliveInterval is the fixed drop-detection cadence — always on per
// the connection model (issue #14); not configurable per session.
const keepaliveInterval = 30 * time.Second

func (s *Session) startKeepalive() {
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		cancel()
		return
	}
	s.keepaliveCancel = cancel
	s.mu.Unlock()
	go s.runKeepalive(ctx, keepaliveInterval)
}

func (s *Session) runKeepalive(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			client, err := s.getClient()
			if err != nil {
				s.attemptReconnect(ctx)
				continue
			}
			_, _, err = client.SendRequest("keepalive@openssh.com", true, nil)
			if err != nil {
				s.attemptReconnect(ctx)
			}
		}
	}
}

func (s *Session) attemptReconnect(ctx context.Context) {
	// Skip if another reconnect (user-driven or keepalive-driven) is already in flight.
	if !s.reconnectMu.TryLock() {
		return
	}
	defer s.reconnectMu.Unlock()

	// The PTYs died with the old transport — drop every shell so the next
	// ssh_shell call respawns fresh (and reports shell_restarted).
	s.closeAllShells()
	s.clearClient()
	s.markState(StateReconnecting, nil)

	backoff := []time.Duration{1 * time.Second, 2 * time.Second, 5 * time.Second, 10 * time.Second, 30 * time.Second}
	for i := 0; ; i++ {
		select {
		case <-ctx.Done():
			return
		default:
		}
		client, jumps, addr, err := dialFinal(s.Spec)
		if err == nil {
			// Return whether the session was still open when the dial
			// landed; if it was closed mid-dial setClient drops the client.
			s.setClient(client, jumps, addr)
			return
		}
		s.markState(StateReconnecting, err)
		wait := backoff[len(backoff)-1]
		if i < len(backoff) {
			wait = backoff[i]
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
