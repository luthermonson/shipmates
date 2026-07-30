//go:build linux

package watchdog

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// prepare makes the child the leader of its own process group, so the whole
// tree can be signalled with a single kill(-pgid). The limits argument is
// accepted for parity with Windows, where it programs Job Object caps; on
// Linux every cap is enforced by the sampler.
//
// TODO: when a cgroup Watcher lands, MaxRSSBytes and MaxProcesses should be
// delegated to memory.max and pids.max instead of polled.
func prepare(cmd *exec.Cmd, _ containment.Limits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// attach is a post-start no-op on Unix; Setpgid at start time is enough.
func attach(*exec.Cmd, containment.Limits) error { return nil }

// release is a no-op on Unix; there is no handle to reclaim.
func release(*exec.Cmd) {}

// killTree signals the child's process group — a negative pid targets the
// group — so children the agent spawned go too, including orphans that
// reparented.
func killTree(cmd *exec.Cmd, kill bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	// Prefer the pgid we asked for; fall back to the pid if getpgid fails
	// (the process may already be gone).
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	return syscall.Kill(-pgid, sig)
}

// sampleRSS reads /proc/<pid>/statm, whose second field is the resident set
// in pages.
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
// seconds.
func sampleCPUSeconds(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseProcStatCPU(string(data))
}

// parseProcStatCPU pulls utime + stime out of a /proc/<pid>/stat body.
//
// The comm field is wrapped in parentheses and may itself contain spaces and
// parentheses, so fields are counted from the LAST ')' rather than split from
// the start.
func parseProcStatCPU(s string) (float64, error) {
	closeIdx := strings.LastIndex(s, ")")
	if closeIdx < 0 {
		return 0, fmt.Errorf("watchdog: malformed stat: %q", s)
	}
	// After ")" the fields are: state ppid pgrp session tty_nr tpgid flags
	// minflt cminflt majflt cmajflt utime stime ... — utime is field 14 of
	// the original file, index 11 here.
	after := strings.Fields(s[closeIdx+1:])
	if len(after) < 13 {
		return 0, fmt.Errorf("watchdog: stat missing utime/stime fields")
	}
	utime, err := strconv.ParseInt(after[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("watchdog: bad utime: %w", err)
	}
	stime, err := strconv.ParseInt(after[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("watchdog: bad stime: %w", err)
	}
	// _SC_CLK_TCK is conventionally 100 on Linux; hardcoding it avoids cgo.
	const clockTicksPerSecond = 100
	return float64(utime+stime) / clockTicksPerSecond, nil
}
