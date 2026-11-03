package sshx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// These tests cover the cold-start gate: the session is published to the map
// before its dial finishes, so parallel first calls must queue on it rather
// than report the half-built session's state. The dial itself is faked — a
// real sshd belongs to test/e2e.

// coldManager returns a Manager whose ssh config has one alias, "cold", and
// whose dial is fn.
func coldManager(t *testing.T, fn dialFunc) *Manager {
	t.Helper()
	dir := t.TempDir()
	sshDir := filepath.Join(dir, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := "" +
		"Host cold\n" +
		"  HostName 127.0.0.1\n" +
		"  User tester\n" +
		"  Port 2222\n"
	if err := os.WriteFile(filepath.Join(sshDir, "config"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	m := NewManager()
	m.dial = fn
	return m
}

func TestConnectParallelCallersShareOneDial(t *testing.T) {
	wantErr := errors.New("dial refused")
	var mu sync.Mutex
	dials := 0
	release := make(chan struct{})
	m := coldManager(t, func(ConnectSpec) (*ssh.Client, []*ssh.Client, string, error) {
		mu.Lock()
		dials++
		mu.Unlock()
		<-release
		return nil, nil, "", wantErr
	})

	const callers = 8
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		go func() {
			_, err := m.Connect(context.Background(), "cold")
			errs <- err
		}()
	}
	// Give every caller time to reach the wait: the goroutines are already
	// runnable, and the dial is parked on release until they are.
	time.Sleep(100 * time.Millisecond)
	close(release)

	for i := 0; i < callers; i++ {
		err := <-errs
		if !errors.Is(err, wantErr) {
			t.Fatalf("caller %d got %v, want the dial's own error", i, err)
		}
		if strings.Contains(err.Error(), "is connecting") {
			t.Fatalf("caller %d saw the half-built session: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if dials != 1 {
		t.Fatalf("dialed %d times for %d parallel first calls, want 1", dials, callers)
	}
	// A failed connect leaves nothing behind, so the next call retries.
	if _, err := m.Get("cold"); err == nil {
		t.Fatal("failed session still in the manager")
	}
}

func TestConnectWaiterHonoursContext(t *testing.T) {
	block := make(chan struct{})
	m := coldManager(t, func(ConnectSpec) (*ssh.Client, []*ssh.Client, string, error) {
		<-block
		return nil, nil, "", errors.New("never dials")
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = m.Connect(context.Background(), "cold")
	}()
	// Wait until the dialing caller has published the session.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if s, err := m.Get("cold"); err == nil && s != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session was never published")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := m.Connect(ctx, "cold"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting Connect = %v, want context.DeadlineExceeded", err)
	}

	close(block)
	<-done
}

func TestConnectRecordsFailureAndDialsAgain(t *testing.T) {
	var dials atomic.Int32
	m := coldManager(t, func(ConnectSpec) (*ssh.Client, []*ssh.Client, string, error) {
		dials.Add(1)
		return nil, nil, "", errors.New("boom raw")
	})
	for i := 0; i < 2; i++ {
		_, err := m.Connect(context.Background(), "cold")
		if err == nil || !strings.Contains(err.Error(), "boom raw") {
			t.Fatalf("connect %d = %v", i, err)
		}
	}
	if dials.Load() != 2 {
		t.Fatalf("dialed %d times, want 2 — a failure must not be cached", dials.Load())
	}
	got, ok := m.LastConnectFailure("cold")
	if !ok || !strings.Contains(got.Error, "boom raw") || got.At.IsZero() {
		t.Fatalf("recorded failure = %+v ok=%v", got, ok)
	}
	m.clearFailure("cold")
	if _, ok := m.LastConnectFailure("cold"); ok {
		t.Fatal("clearFailure left the error in place")
	}
}
