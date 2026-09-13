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
// Linux every cap is enforced by the sampler, which reads the whole process
// group (see sampleTreeRSS / sampleTreeCPUSeconds), matching the tree scope
// killTree signals and the Windows Job Object enforces.
//
// Sampling is the deliberate choice, not a placeholder for cgroup delegation:
// shipmates dropped its Linux-only cgroup containment precisely so every
// platform gets the same bounds. Delegating memory.max and pids.max here would
// need privileges or a delegated scope, and would make Linux the only place
// enforcement behaves differently.
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
// in pages. This is the single-process primitive; sampleTreeRSS sums it across
// the whole process group, which is what the poll loop actually enforces.
func sampleRSS(pid int) (int64, error) {
	pages, err := readStatmRSSPages(pid)
	if err != nil {
		return 0, err
	}
	return pages * int64(os.Getpagesize()), nil
}

// readStatmRSSPages returns the resident pages of a single pid from
// /proc/<pid>/statm (field 2).
func readStatmRSSPages(pid int) (int64, error) {
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
	return pages, nil
}

// sampleCPUSeconds reads /proc/<pid>/stat and returns utime + stime in
// seconds for a single pid. sampleTreeCPUSeconds sums it across the group.
func sampleCPUSeconds(pid int) (float64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	return parseProcStatCPU(string(data))
}

// sampleTreeRSS sums the resident set of every process in the child's process
// group. pgid is the group-leader pid (the root the watchdog launched). Walks
// /proc, reads each process's stat to learn its pgrp (field 5), and for the
// ones in the group reads statm for RSS. Summing the group — not just the root
// — is what keeps a memory-hogging grandchild from escaping the cap on Linux,
// matching the kernel-enforced whole-tree scope on Windows.
func sampleTreeRSS(pgid int) (int64, error) {
	procs, err := readGroupProcRSS(pgid)
	if err != nil {
		return 0, err
	}
	pages, matched := sumGroupRSSPages(pgid, procs)
	if matched == 0 {
		// The group leader (and thus the tree) is gone: a failed sample, not a
		// real 0 — skip the tick rather than fake a reading below the cap.
		return 0, fmt.Errorf("watchdog: no processes found in group %d", pgid)
	}
	return pages * int64(os.Getpagesize()), nil
}

// sampleTreeCPUSeconds sums utime + stime across every process in the child's
// process group. Same walk as sampleTreeRSS; the CPU figures come from the
// same stat read used for the pgrp, so no extra file is opened.
func sampleTreeCPUSeconds(pgid int) (float64, error) {
	procs, err := readGroupProcCPU(pgid)
	if err != nil {
		return 0, err
	}
	seconds, matched := sumGroupCPUSeconds(pgid, procs)
	if matched == 0 {
		return 0, fmt.Errorf("watchdog: no processes found in group %d", pgid)
	}
	return seconds, nil
}

// readGroupProcRSS walks /proc and returns a procRSS for each live process
// whose process group is pgid. A process that races its own exit between the
// stat and statm reads is skipped, not fatal.
func readGroupProcRSS(pgid int) ([]procRSS, error) {
	pids, err := listProcPids()
	if err != nil {
		return nil, err
	}
	var procs []procRSS
	for _, pid := range pids {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue // gone between listing and reading
		}
		pg, err := parseProcStatPgrp(string(stat))
		if err != nil || pg != pgid {
			continue
		}
		pages, err := readStatmRSSPages(pid)
		if err != nil {
			continue
		}
		procs = append(procs, procRSS{pgrp: pg, rssPages: pages})
	}
	return procs, nil
}

// readGroupProcCPU walks /proc and returns a procCPU for each live process
// whose process group is pgid, reading pgrp and CPU time from one stat read.
func readGroupProcCPU(pgid int) ([]procCPU, error) {
	pids, err := listProcPids()
	if err != nil {
		return nil, err
	}
	var procs []procCPU
	for _, pid := range pids {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil {
			continue
		}
		s := string(stat)
		pg, err := parseProcStatPgrp(s)
		if err != nil || pg != pgid {
			continue
		}
		secs, err := parseProcStatCPU(s)
		if err != nil {
			continue
		}
		procs = append(procs, procCPU{pgrp: pg, seconds: secs})
	}
	return procs, nil
}

// listProcPids returns the numeric pid directories under /proc.
func listProcPids() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // /proc has non-pid entries like "self", "stat", "cpuinfo"
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// parseProcStatPgrp pulls the process group id (field 5) out of a
// /proc/<pid>/stat body. Like parseProcStatCPU it counts fields from the LAST
// ')', because the comm field is parenthesised and may contain spaces and
// parentheses. After ')' the fields are: state ppid pgrp ... — pgrp is index 2.
func parseProcStatPgrp(s string) (int, error) {
	closeIdx := strings.LastIndex(s, ")")
	if closeIdx < 0 {
		return 0, fmt.Errorf("watchdog: malformed stat: %q", s)
	}
	after := strings.Fields(s[closeIdx+1:])
	if len(after) < 3 {
		return 0, fmt.Errorf("watchdog: stat missing pgrp field")
	}
	pgrp, err := strconv.Atoi(after[2])
	if err != nil {
		return 0, fmt.Errorf("watchdog: bad pgrp: %w", err)
	}
	return pgrp, nil
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
