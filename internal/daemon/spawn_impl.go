package daemon

import (
	"os/exec"
	"syscall"
	"time"
)

const defaultStartTimeout = 10 * time.Second

// detachDaemon runs the daemon in its own session so it survives the
// caller's exit.
func detachDaemon(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
