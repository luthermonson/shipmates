//go:build windows

package codexapp

import (
	"os"
	"os/exec"
)

func configureProcessGroup(*exec.Cmd) {}

func signalProcessGroup(pid int, kill bool) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if kill {
		return process.Kill()
	}
	return nil
}

func openProcessIdentity(pid int) (int, error)      { return 0, os.ErrInvalid }
func signalProcessIdentity(fd int, kill bool) error { return nil }
func closeProcessIdentity(fd int) error             { return nil }
