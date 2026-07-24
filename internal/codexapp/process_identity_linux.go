//go:build linux

package codexapp

import "golang.org/x/sys/unix"

// openProcessIdentity returns a Linux pidfd — an atomic handle that
// remains bound to the original process even across PID recycling.
func openProcessIdentity(pid int) (int, error) { return unix.PidfdOpen(pid, 0) }

func signalProcessIdentity(fd int, kill bool) error {
	signal := unix.SIGINT
	if kill {
		signal = unix.SIGKILL
	}
	return unix.PidfdSendSignal(fd, signal, nil, 0)
}

func closeProcessIdentity(fd int) error { return unix.Close(fd) }
