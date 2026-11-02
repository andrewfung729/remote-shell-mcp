package sshx

import (
	"fmt"
	"time"
)

// ConnectSpec is resolved entirely from the daemon user's ~/.ssh/config —
// callers (tools, WebUI) never construct it with connection details; the
// manager fills every field from the Host entry that matches the alias.
// It stays a struct (not a bare alias) so the resolved values travel with
// the session for reconnects.
type ConnectSpec struct {
	User           string     `json:"user"`
	Addresses      []string   `json:"addresses"`
	JumpHosts      []JumpHost `json:"jump_hosts,omitempty"`
	KnownHostsPath string     `json:"known_hosts_path,omitempty"`
	Timeout        Duration   `json:"timeout,omitempty"`
	// Auth is resolved, never supplied: Connect fills it from the daemon
	// user's ~/.ssh/config (IdentityFile / IdentityAgent). An agent cannot
	// pick credentials — a spec whose host has no resolvable auth fails.
	Auth AuthSpec `json:"-"`
}

type JumpHost struct {
	User           string   `json:"user"`
	Addresses      []string `json:"addresses"`
	KnownHostsPath string   `json:"known_hosts_path,omitempty"`
	Timeout        Duration `json:"timeout,omitempty"`
	Auth           AuthSpec `json:"-"`
}

type Duration time.Duration

func (d Duration) Std() time.Duration { return time.Duration(d) }

func (d Duration) MarshalJSON() ([]byte, error) {
	return []byte(`"` + time.Duration(d).String() + `"`), nil
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	if len(b) >= 2 && b[0] == '"' {
		s := string(b[1 : len(b)-1])
		v, err := time.ParseDuration(s)
		if err != nil {
			return err
		}
		if v < 0 {
			return fmt.Errorf("duration %q is negative", s)
		}
		*d = Duration(v)
		return nil
	}
	if len(b) == 0 {
		return fmt.Errorf("empty duration")
	}
	if len(b) > 19 { // int64 has at most 19 decimal digits
		return fmt.Errorf("duration %q overflows int64 nanoseconds", string(b))
	}
	var ns int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return fmt.Errorf("duration %q is not a string or non-negative integer (nanoseconds)", string(b))
		}
		next := ns*10 + int64(c-'0')
		if next < ns {
			return fmt.Errorf("duration %q overflows int64 nanoseconds", string(b))
		}
		ns = next
	}
	*d = Duration(time.Duration(ns))
	return nil
}
