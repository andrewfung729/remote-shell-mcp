package state

import (
	"log/slog"

	"github.com/jaenster/remote-shell-mcp/internal/sshx"
)

func Capture(ssh *sshx.Manager, disabled []string) *Snapshot {
	snap := &Snapshot{Version: 1, DisabledTools: disabled}

	for _, s := range ssh.Sessions() {
		if !s.Spec.Persistent {
			continue
		}
		snap.SSHSessions = append(snap.SSHSessions, SSHSessionRecord{
			ID:   s.ID,
			Spec: s.Spec,
		})
	}

	return snap
}

func Restore(snap *Snapshot, ssh *sshx.Manager, log *slog.Logger) {
	var sshOK, sshFail int
	for _, rec := range snap.SSHSessions {
		if _, err := ssh.Connect(rec.ID, rec.Spec); err != nil {
			sshFail++
			log.Warn("restore ssh session failed", "id", rec.ID, "err", err)
		} else {
			sshOK++
		}
	}
	// One summary line instead of one INFO per restored session. Failures
	// still log individually above so the operator can see *which* one
	// didn't come back; successes are summarized to keep restart logs
	// readable when there are hundreds of persistent sessions.
	log.Info("state restored",
		"ssh_sessions_ok", sshOK, "ssh_sessions_failed", sshFail,
	)
}
