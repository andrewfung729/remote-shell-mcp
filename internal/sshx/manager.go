package sshx

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

// dialFunc is the connect seam: dialFinal in production, a fake in tests that
// check the concurrency of a cold start without an sshd.
type dialFunc func(spec ConnectSpec) (client *ssh.Client, jumps []*ssh.Client, addr string, err error)

type Manager struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	failures map[string]ConnectFailure // last failed dial per alias; not a negative cache
	dial     dialFunc
}

func NewManager() *Manager {
	return &Manager{
		sessions: map[string]*Session{},
		failures: map[string]ConnectFailure{},
		dial:     dialFinal,
	}
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
// A second caller that arrives while the first dial is still in flight waits
// for it (bounded by ctx) and gets the same session or the same error — a cold
// host's parallel first calls share one connection instead of failing with
// "session is connecting".
func (m *Manager) Connect(ctx context.Context, host string) (*Session, error) {
	if host == "" {
		return nil, errors.New("host is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.RLock()
	existing, ok := m.sessions[host]
	m.mu.RUnlock()
	if ok {
		return existing.waitReady(ctx)
	}

	if !hasConfigEntry(host) {
		return m.failConnect(host, fmt.Errorf("%w: %s", errConfigIncomplete, errNoSSHConfigEntry.Error()))
	}
	spec := ConnectSpec{Addresses: []string{host}}
	resolveFromSSHConfig(&spec)
	if spec.User == "" {
		return m.failConnect(host, fmt.Errorf("%w: the ssh_config entry doesn't set User (add one to ~/.ssh/config)", errConfigIncomplete))
	}
	for _, j := range spec.JumpHosts {
		if j.User == "" {
			return m.failConnect(host, fmt.Errorf("%w: a jump host has no User in its ssh_config entry", errConfigIncomplete))
		}
	}
	id := host

	m.mu.Lock()
	if existing, exists := m.sessions[id]; exists {
		m.mu.Unlock()
		return existing.waitReady(ctx)
	}
	sess := &Session{
		ID:     id,
		Spec:   spec,
		state:  StateConnecting,
		shells: map[string]*Shell{},
		ready:  make(chan struct{}),
	}
	m.sessions[id] = sess
	m.mu.Unlock()

	// From here the session is visible to parallel callers, which wait on
	// ready — so every path out of this function must settle it.
	client, jumps, addr, err := m.dial(spec)
	if err != nil {
		// Record the raw error, then drop the session. The next call dials
		// again — a failure is never a cached answer.
		m.noteFailure(id, err)
		m.deleteIf(id, sess)
		sess.settle(err)
		return nil, err
	}
	if !sess.setClient(client, jumps, addr) {
		// Session was disconnected mid-dial. setClient already closed the
		// new connection — clean up the map entry if it's still there.
		m.deleteIf(id, sess)
		lost := errors.New("session was disconnected during connect")
		m.noteFailure(id, lost)
		sess.settle(lost)
		return nil, lost
	}

	sess.startKeepalive()
	sess.startReaper()
	m.clearFailure(id)
	sess.settle(nil)
	return sess, nil
}

// ConnectFailure is the raw cause of the last failed dial for an alias,
// shown on the WebUI hosts page. A successful connect clears it. It does
// not short-circuit the next dial.
type ConnectFailure struct {
	Error string
	At    time.Time
}

func (m *Manager) failConnect(host string, err error) (*Session, error) {
	m.noteFailure(host, err)
	return nil, err
}

func (m *Manager) noteFailure(host string, err error) {
	if err == nil || host == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failures == nil {
		m.failures = map[string]ConnectFailure{}
	}
	m.failures[host] = ConnectFailure{Error: err.Error(), At: time.Now()}
}

func (m *Manager) clearFailure(host string) {
	m.mu.Lock()
	delete(m.failures, host)
	m.mu.Unlock()
}

// LastConnectFailure returns the raw error recorded for alias, if any.
func (m *Manager) LastConnectFailure(host string) (ConnectFailure, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	f, ok := m.failures[host]
	return f, ok
}

// deleteIf drops the map entry only when it is still this session — a
// concurrent Disconnect (or a later Connect for the same alias) may already
// have replaced or removed it.
func (m *Manager) deleteIf(id string, sess *Session) {
	m.mu.Lock()
	if existing, ok := m.sessions[id]; ok && existing == sess {
		delete(m.sessions, id)
	}
	m.mu.Unlock()
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

	client, jumps, addr, err := m.dial(s.Spec)
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
