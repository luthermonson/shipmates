//go:build unix

package codexapp

import (
	"os/exec"
	"syscall"
)

func configureProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func signalProcessGroup(pid int, kill bool) error {
	signal := syscall.SIGINT
	if kill {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-pid, signal)
}
