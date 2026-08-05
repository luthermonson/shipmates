//go:build unix && !linux

package codexapp

import "syscall"

// openProcessIdentity on non-Linux unix (darwin, BSD) returns the raw PID
// as the identity token. Unlike Linux pidfd, this is theoretically subject
// to PID recycling — acceptable in shipmates' single-adapter-per-child
// model where the window between adapter Start and any signal is short
// (turn-bounded), and where the operator's own process, not a foreign
// one, would end up recycled into the slot.
func openProcessIdentity(pid int) (int, error) { return pid, nil }

func signalProcessIdentity(fd int, kill bool) error {
	signal := syscall.SIGINT
	if kill {
		signal = syscall.SIGKILL
	}
	return syscall.Kill(fd, signal)
}

func closeProcessIdentity(fd int) error { return nil }
