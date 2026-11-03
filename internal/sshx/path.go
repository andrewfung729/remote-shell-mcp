package sshx

import (
	"fmt"
	"path"
	"strings"
)

// Remote paths arrive as the caller typed them: absolute, relative to the
// remote home (which is where an SFTP session starts), or with a leading "~".
// "~" is shell syntax, not SFTP syntax — the protocol expands nothing — so the
// daemon resolves it here, once, before any file operation touches the wire.
// The resolved form is also what the read-before-edit guard keys on, so a read
// of "~/notes.txt" and an edit of "/home/me/notes.txt" are the same file.

// NeedsHome reports whether p starts with the "~" form ExpandHome resolves.
// Only bare "~" and "~/" are expanded: "~user/..." is left exactly as given,
// because another user's home lives in the remote passwd database and asking
// for it is a different request from asking for mine.
func NeedsHome(p string) bool {
	return p == "~" || strings.HasPrefix(p, "~/")
}

// ExpandHome replaces a leading "~" with home. A path without one — and any
// path when home is unknown — is returned unchanged, so callers may apply it
// unconditionally. Trailing slashes on home are tolerated.
func ExpandHome(home, p string) string {
	if home == "" {
		return p
	}
	if p == "~" {
		return home
	}
	rest, ok := strings.CutPrefix(p, "~/")
	if !ok {
		return p
	}
	return strings.TrimSuffix(home, "/") + "/" + rest
}

// HomeDir returns the remote user's home directory, asked of the SFTP server
// once per connection and cached until the next reconnect (a new transport may
// be a different machine behind the same alias). The server's working
// directory at open time is that home, so Getwd reports whatever home sshd
// itself uses — no passwd parsing, no assumption about layout.
func (s *Session) HomeDir() (string, error) {
	s.homeMu.Lock()
	home := s.home
	s.homeMu.Unlock()
	if home != "" {
		return home, nil
	}
	c, err := s.sftpClient()
	if err != nil {
		return "", err
	}
	wd, err := c.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve remote home: %w", err)
	}
	if !path.IsAbs(wd) {
		return "", fmt.Errorf("resolve remote home: server reported %q", wd)
	}
	s.homeMu.Lock()
	s.home = wd
	s.homeMu.Unlock()
	return wd, nil
}

// ResolvePath turns a caller-supplied remote path into the one SFTP calls use:
// a leading "~" is expanded against the remote home, anything else passes
// through untouched (relative paths stay relative to that same home).
func (s *Session) ResolvePath(p string) (string, error) {
	if !NeedsHome(p) {
		return p, nil
	}
	home, err := s.HomeDir()
	if err != nil {
		return "", err
	}
	return ExpandHome(home, p), nil
}
