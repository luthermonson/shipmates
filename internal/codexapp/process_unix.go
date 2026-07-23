//go:build !windows

package codexapp

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

func configureProcessGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func signalProcessGroup(pid int, kill bool) error {
	signal := syscall.SIGINT
	if kill {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(-pid, signal)
}

func openProcessIdentity(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

func signalProcessIdentity(fd int, kill bool) error {
	signal := unix.SIGINT
	if kill {
		signal = unix.SIGKILL
	}
	return unix.PidfdSendSignal(fd, signal, nil, 0)
}

func closeProcessIdentity(fd int) error { return unix.Close(fd) }
