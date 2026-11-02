package sshx

// Shell listing/introspection for the WebUI. The Shell type itself (and the
// command-execution layer) lives in shellcmd.go — this file keeps the read
// paths the WebUI and close-paths use so shellcmd.go stays about running
// commands.

// ShellInfo is the WebUI's view of one shell.
type ShellInfo struct {
	Name     string `json:"name"`
	Owner    string `json:"owner,omitempty"`
	Degraded bool   `json:"degraded,omitempty"`
}

// AllShells returns every shell on the session — for the WebUI, which shows
// the operator all shells across all clients.
func (sess *Session) AllShells() []ShellInfo {
	sess.shellsMu.Lock()
	defer sess.shellsMu.Unlock()
	out := make([]ShellInfo, 0, len(sess.shells))
	for _, sh := range sess.shells {
		out = append(out, sh.Info())
	}
	return out
}

func (sh *Shell) Info() ShellInfo {
	return ShellInfo{Name: sh.Name, Owner: sh.Owner, Degraded: sh.Degraded}
}

// closeAllShells drops every shell — called on session close so a dead PTY
// is never handed to a caller.
func (s *Session) closeAllShells() {
	s.shellsMu.Lock()
	for id, sh := range s.shells {
		_ = sh.Close()
		delete(s.shells, id)
	}
	s.shellsMu.Unlock()
}
