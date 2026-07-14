//go:build windows

package client

import (
	"os/exec"
	"syscall"
)

// detach starts the server in its own process group, decoupled from the
// caller's console so Ctrl-C in the launching terminal does not kill it directly.
func detach(cmd *exec.Cmd) {
	const (
		detachedProcess    = 0x00000008
		createNewProcGroup = 0x00000200
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcGroup}
}
