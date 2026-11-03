package sshx

import (
	"context"
	"time"
)

// shellIdleTTL is how long a shell with no ssh_shell call and no command in
// flight stays open. Not configurable: a throwaway name is the way to run a
// one-off, and this is what stops those PTYs piling up. A command that is
// still running is never reaped, however long it takes.
const shellIdleTTL = 30 * time.Minute

// shellSnap is one shell's reap inputs. shellsToReap is a pure function of
// these plus now and the TTL — the sweep loop stays separate.
type shellSnap struct {
	name string
	last time.Time
	busy bool
}

// shellsToReap returns the names idle strictly longer than ttl. A shell with
// a command in flight is kept, including the one named default.
func shellsToReap(shells []shellSnap, now time.Time, ttl time.Duration) []string {
	var names []string
	for _, sh := range shells {
		if sh.busy {
			continue
		}
		if now.Sub(sh.last) > ttl {
			names = append(names, sh.name)
		}
	}
	return names
}

func (s *Session) startReaper() {
	s.mu.Lock()
	if s.reapCancel != nil || s.closed.Load() {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.reapCancel = cancel
	s.mu.Unlock()
	go s.reapLoop(ctx)
}

func (s *Session) reapLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.reapIdle(time.Now())
		}
	}
}

// reapIdle closes shells the pure decision says are idle. The check and the
// delete share shellsMu so a call that just touched the shell, or a command
// that started, cannot lose the race.
func (s *Session) reapIdle(now time.Time) {
	s.shellsMu.Lock()
	defer s.shellsMu.Unlock()
	for name, sh := range s.shells {
		last, busy := sh.activity()
		if len(shellsToReap([]shellSnap{{name: name, last: last, busy: busy}}, now, shellIdleTTL)) == 0 {
			continue
		}
		delete(s.shells, name)
		_ = sh.Close()
	}
}
