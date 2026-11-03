package sshx

import (
	"testing"
	"time"
)

func TestShellsToReap(t *testing.T) {
	now := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	ttl := 30 * time.Minute
	idle := now.Add(-ttl - time.Second)
	fresh := now.Add(-ttl)
	recent := now.Add(-time.Minute)
	shells := []shellSnap{
		{name: "scratch", last: idle, busy: false},
		{name: "default", last: idle, busy: false},
		{name: "edge", last: fresh, busy: false},
		{name: "recent", last: recent, busy: false},
		{name: "build", last: idle, busy: true},
	}
	got := shellsToReap(shells, now, ttl)
	if len(got) != 2 || got[0] != "scratch" || got[1] != "default" {
		t.Fatalf("reaped %v, want scratch and default (in flight, exact TTL, and recent stay)", got)
	}
}
