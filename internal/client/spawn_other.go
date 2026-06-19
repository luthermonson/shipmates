//go:build !windows

package client

import (
	"os/exec"
	"syscall"
)

// detach puts the server in its own session/process group so it survives the
// caller exiting and isn't hit by the caller's terminal signals.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
