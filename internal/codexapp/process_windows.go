//go:build windows

package codexapp

import (
	"os"
	"os/exec"

	"golang.org/x/sys/windows"
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

// openProcessIdentity returns a real Windows process handle, which is the
// closest analogue Windows has to a Linux pidfd and carries the same property
// that matters: the handle names one process object for as long as it is held, so
// a PID recycled into the same slot cannot be signalled by mistake.
//
// This used to return os.ErrInvalid. That was not a harmless stub — Start treats
// a failure here as fatal, kills the child it has just spawned, and reports
// Internal, so *every* Codex app-server launch on Windows failed with "the Codex
// app-server could not be started", taking `open`, `live`, `tell`, `feed`,
// `show`, and `interrupt` with it. It also made the whole livesession suite
// unrunnable on Windows.
func openProcessIdentity(pid int) (int, error) {
	h, err := windows.OpenProcess(
		windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION,
		false, uint32(pid))
	if err != nil {
		return 0, err
	}
	return int(h), nil
}

// signalProcessIdentity terminates the process behind the handle.
//
// The graceful half has no Windows equivalent: SIGINT reaches a child through a
// shared console, and these children are spawned with pipes and no console of
// their own, so there is nothing to deliver. Reporting success for a no-op is
// deliberate and matches signalProcessGroup above — Adapter.Close reaches
// GracefulTerminate only after it has already closed the child's stdin and
// waited, which is the actual graceful path on this platform, and escalates to
// ForceKill when that does not finish.
//
// An already-exited process is reported as successfully killed, because on unix
// SIGKILL to an unreaped child succeeds and callers treat a kill error as
// CleanupFailed. Without the check TerminateProcess would return
// ERROR_ACCESS_DENIED for a child that had already done exactly what was wanted.
func signalProcessIdentity(fd int, kill bool) error {
	if !kill {
		return nil
	}
	h := windows.Handle(fd)
	if state, err := windows.WaitForSingleObject(h, 0); err == nil && state == uint32(windows.WAIT_OBJECT_0) {
		return nil
	}
	return windows.TerminateProcess(h, 1)
}

func closeProcessIdentity(fd int) error { return windows.CloseHandle(windows.Handle(fd)) }
