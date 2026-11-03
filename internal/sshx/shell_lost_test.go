package sshx

import (
	"testing"
	"time"
)

// lostSession is a session that never dials. spawnShell stands in for the PTY.
func lostSession(m *Manager, host string) *Session {
	s := &Session{ID: host, shells: map[string]*Shell{}}
	s.markLost = func(name string) { m.markShellLost(host, name) }
	s.takeLost = func(name string) bool { return m.takeShellLost(host, name) }
	s.spawnShell = func(name, owner string) (*Shell, error) {
		return &Shell{Name: name, Owner: owner, out: newShellReader(), doneCh: make(chan struct{}), lastActivity: time.Now()}, nil
	}
	return s
}

func markClosed(sh *Shell) {
	sh.mu.Lock()
	sh.closed = true
	sh.mu.Unlock()
}

func TestOpenOrRespawnFirstOpenIsNotRestart(t *testing.T) {
	m := NewManager()
	s := lostSession(m, "gis-president")
	for _, name := range []string{"default", "b"} {
		sh, restarted, err := s.OpenOrRespawn(name, "agent")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if restarted {
			t.Fatalf("%s: first open reported restart", name)
		}
		if sh.Name != name {
			t.Fatalf("%s: opened %q", name, sh.Name)
		}
	}
	_, restarted, err := s.OpenOrRespawn("default", "agent")
	if err != nil || restarted {
		t.Fatalf("live shell: restarted=%v err=%v", restarted, err)
	}
}

func TestOpenOrRespawnRestartAfterLoss(t *testing.T) {
	m := NewManager()
	s := lostSession(m, "gis-president")
	if _, _, err := s.OpenOrRespawn("default", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.OpenOrRespawn("b", "agent"); err != nil {
		t.Fatal(err)
	}

	markClosed(s.shells["default"])
	_, restarted, err := s.OpenOrRespawn("default", "agent")
	if err != nil || !restarted {
		t.Fatalf("dead shell: restarted=%v err=%v", restarted, err)
	}
	_, restarted, err = s.OpenOrRespawn("default", "agent")
	if err != nil || restarted {
		t.Fatalf("restart announced twice: restarted=%v err=%v", restarted, err)
	}

	markClosed(s.shells["b"])
	s.DropShell("b", "agent")
	_, restarted, err = s.OpenOrRespawn("b", "agent")
	if err != nil || !restarted {
		t.Fatalf("reset: restarted=%v err=%v", restarted, err)
	}

	markClosed(s.shells["default"])
	s.shells["default"].lastActivity = time.Now().Add(-time.Hour)
	s.reapIdle(time.Now())
	_, restarted, err = s.OpenOrRespawn("default", "agent")
	if err != nil || !restarted {
		t.Fatalf("reap: restarted=%v err=%v", restarted, err)
	}

	// A name that was never opened stays quiet, even after other shells died.
	_, restarted, err = s.OpenOrRespawn("scratch", "agent")
	if err != nil || restarted {
		t.Fatalf("scratch: restarted=%v err=%v", restarted, err)
	}
}

func TestShellLostSurvivesNewSession(t *testing.T) {
	m := NewManager()
	old := lostSession(m, "gis-president")
	if _, _, err := old.OpenOrRespawn("default", "agent"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := old.OpenOrRespawn("b", "agent"); err != nil {
		t.Fatal(err)
	}
	markClosed(old.shells["default"])
	markClosed(old.shells["b"])
	old.close()

	next := lostSession(m, "gis-president")
	for _, name := range []string{"default", "b"} {
		_, restarted, err := next.OpenOrRespawn(name, "agent")
		if err != nil || !restarted {
			t.Fatalf("%s after session close: restarted=%v err=%v", name, restarted, err)
		}
	}
	_, restarted, err := next.OpenOrRespawn("scratch", "agent")
	if err != nil || restarted {
		t.Fatalf("scratch after session close: restarted=%v err=%v", restarted, err)
	}
}
