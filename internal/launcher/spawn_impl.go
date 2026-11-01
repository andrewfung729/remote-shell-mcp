package launcher

import (
	"os/exec"
	"syscall"
	"time"
)

const defaultStartTimeout = 10 * time.Second

// detach runs the daemon in its own session so it survives the launcher's exit.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
