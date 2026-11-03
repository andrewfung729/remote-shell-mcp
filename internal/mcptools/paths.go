package mcptools

import (
	"fmt"
	"os"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

// Path handling for both ends of a transfer. Remote paths are "~"-aware and
// spelled once: the resolved form is what the SFTP calls use AND what the
// read-before-edit guard keys on, so "~/notes.txt" and "/home/me/notes.txt"
// are the same file to a read followed by an edit. Local paths are on the
// daemon's own filesystem and resolve against the daemon user's home.

// remotePath resolves a caller-supplied remote path against the session: a
// leading "~" becomes the remote home dir, everything else is passed through.
func remotePath(s *sshx.Session, raw string) (string, error) {
	return s.ResolvePath(raw)
}

// localPath expands a leading "~" against the daemon user's home. It reuses
// sshx's "~" rule on purpose — one definition of the syntax on both ends — and
// the daemon only ever runs on macOS or Linux, so "/" is the separator.
func localPath(raw string) (string, error) {
	if !sshx.NeedsHome(raw) {
		return raw, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("expand %q: %w", raw, err)
	}
	return sshx.ExpandHome(home, raw), nil
}
