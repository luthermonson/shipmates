//go:build windows

package project

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

func tryDispatchFileLock(f *os.File) (bool, error) {
	const lockfileFailImmediately = 0x1
	const lockfileExclusiveLock = 0x2
	var overlapped windows.Overlapped
	err := windows.LockFileEx(windows.Handle(f.Fd()), lockfileFailImmediately|lockfileExclusiveLock, 0, 1, 0, &overlapped)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	} // ERROR_LOCK_VIOLATION
	return err == nil, err
}
func unlockDispatchFile(f *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &overlapped)
}
func dispatchProcessAlive(pid int) (bool, error) {
	const processQueryLimitedInformation = 0x1000
	h, err := windows.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
		return false, nil
	} // ERROR_INVALID_PARAMETER
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true, nil
	} // ERROR_ACCESS_DENIED
	if err != nil {
		return false, err
	}
	defer windows.CloseHandle(h)
	var exitCode uint32
	if err := windows.GetExitCodeProcess(h, &exitCode); err != nil {
		return false, err
	}
	return exitCode == 259, nil // STILL_ACTIVE
}
