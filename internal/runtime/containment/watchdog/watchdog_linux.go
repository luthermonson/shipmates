//go:build linux

package watchdog

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

// prepare arranges for the child to become the leader of its own process
// group so we can signal the whole tree with a single -pgid kill.
func prepare(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// attach is a post-start no-op on Unix; Setpgid at Start time is enough.
func attach(*exec.Cmd) error { return nil }

// killTree sends SIGTERM (kill=false) or SIGKILL (kill=true) to the child's
// process group. Using a negative pid targets the group, so orphaned
// children the AI may have spawned are also killed.
func killTree(cmd *exec.Cmd, kill bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	// Prefer the pgid we requested; fall back to the pid if getpgid fails.
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	return syscall.Kill(-pgid, sig)
}

// sampleRSS reads /proc/<pid>/statm which returns memory usage in pages.
// Field 1 (resident) is the RSS. Multiply by the OS page size for bytes.
// On kernel error (process already reaped, permission), returns 0, err.
func sampleRSS(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0, fmt.Errorf("watchdog: malformed statm: %q", data)
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return 0, err
	}
	return pages * int64(os.Getpagesize()), nil
}

// sampleCPUSeconds reads /proc/<pid>/stat and returns utime + stime in
// seconds. The values in the file are in clock ticks; we divide by
// _SC_CLK_TCK, conventionally 100 on Linux.
func sampleCPUSeconds(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// The comm field can contain spaces + parens; scan from the LAST ')'.
	s := string(data)
	closeIdx := strings.LastIndex(s, ")")
	if closeIdx < 0 {
		return 0, fmt.Errorf("watchdog: malformed stat: %q", s)
	}
	after := strings.Fields(s[closeIdx+1:])
	// After ")", the fields are: state ppid pgrp session tty_nr tpgid ...
	// utime is field 14 in the original file → field 11 in `after`.
	if len(after) < 15 {
		return 0, fmt.Errorf("watchdog: stat missing utime/stime fields")
	}
	utime, err := strconv.ParseInt(after[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseInt(after[12], 10, 64)
	if err != nil {
		return 0, err
	}
	// _SC_CLK_TCK is conventionally 100 on Linux; hardcoding avoids cgo.
	// If we later care about exotic tick rates, expose an override.
	const clockTicksPerSecond = 100
	return float64(utime+stime) / clockTicksPerSecond, nil
}
