//go:build darwin

package watchdog

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// prepare puts the child in its own process group, same as Linux. Limits are
// accepted for signature parity with Windows and enforced by the sampler on
// macOS, which reads the whole process group (see sampleTreeRSS /
// sampleTreeCPUSeconds), matching the tree scope killTree signals.
func prepare(cmd *exec.Cmd, _ containment.Limits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

// attach is a post-start no-op on Unix.
func attach(*exec.Cmd, containment.Limits) error { return nil }

// release is a no-op on Unix; there is no handle to reclaim.
func release(*exec.Cmd) {}

// killTree signals the child's process group.
func killTree(cmd *exec.Cmd, kill bool) error {
	if cmd.Process == nil {
		return nil
	}
	sig := syscall.SIGTERM
	if kill {
		sig = syscall.SIGKILL
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		pgid = cmd.Process.Pid
	}
	return syscall.Kill(-pgid, sig)
}

// sampleRSS shells out to `ps -o rss=`. macOS has no /proc and no pure-Go
// path to task_info, so ps is the pragmatic cgo-free answer. Cost is one fork
// per poll interval — acceptable at the 500ms default, worth revisiting if
// the cadence tightens.
//
// This is the single-process primitive; sampleTreeRSS sums the whole process
// group, which is what the poll loop enforces.
func sampleRSS(pid int) (int64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" {
		return 0, fmt.Errorf("watchdog: ps returned no rss for pid %d", pid)
	}
	kb, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, err
	}
	return kb * 1024, nil
}

// sampleCPUSeconds shells out to `ps -o time=`, which prints hh:mm:ss.hh, for
// a single pid. sampleTreeCPUSeconds sums it across the group.
func sampleCPUSeconds(pid int) (float64, error) {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return parseCPUTime(strings.TrimSpace(string(out)))
}

// sampleTreeRSS sums the resident set of every process in the child's process
// group. pgid is the group-leader pid the watchdog launched. macOS ps has no
// dependable process-group selector, so it snapshots all processes with their
// pgid (`ps -Ao pgid=,rss=`) and sums the rows whose pgid matches. Summing the
// group — not just the root — keeps a memory-hogging grandchild from escaping
// the cap, matching the whole-tree scope enforced on Windows.
func sampleTreeRSS(pgid int) (int64, error) {
	out, err := exec.Command("ps", "-Ao", "pgid=,rss=").Output()
	if err != nil {
		return 0, err
	}
	total, matched, err := sumGroupRSSFromPS(pgid, string(out))
	if err != nil {
		return 0, err
	}
	if matched == 0 {
		// The whole group is gone: a failed sample, not a real 0.
		return 0, fmt.Errorf("watchdog: no processes found in group %d", pgid)
	}
	return total, nil
}

// sampleTreeCPUSeconds sums CPU seconds across every process in the child's
// process group, via `ps -Ao pgid=,time=`.
func sampleTreeCPUSeconds(pgid int) (float64, error) {
	out, err := exec.Command("ps", "-Ao", "pgid=,time=").Output()
	if err != nil {
		return 0, err
	}
	total, matched, err := sumGroupCPUFromPS(pgid, string(out))
	if err != nil {
		return 0, err
	}
	if matched == 0 {
		return 0, fmt.Errorf("watchdog: no processes found in group %d", pgid)
	}
	return total, nil
}

// sumGroupRSSFromPS parses `ps -Ao pgid=,rss=` output — one "pgid rss(KiB)" row
// per process — and totals RSS (in bytes) over the rows whose pgid matches,
// returning the total and how many rows matched. Rows for other groups, and
// rows whose pgid column is unparseable (some other process), are skipped; a
// malformed rss for a matching row is an error so the tick is skipped rather
// than under-counted. Factored out of the ps call so it is testable on any host.
func sumGroupRSSFromPS(pgid int, out string) (int64, int, error) {
	var total int64
	matched := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pg, err := strconv.Atoi(fields[0])
		if err != nil || pg != pgid {
			continue
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, 0, fmt.Errorf("watchdog: bad rss %q: %w", fields[1], err)
		}
		total += kb * 1024
		matched++
	}
	return total, matched, nil
}

// sumGroupCPUFromPS parses `ps -Ao pgid=,time=` output and totals CPU seconds
// over the rows whose pgid matches, returning the total and the match count.
// The time column may itself contain no spaces (hh:mm:ss.hh), so each matching
// row's remaining fields are rejoined before parseCPUTime.
func sumGroupCPUFromPS(pgid int, out string) (float64, int, error) {
	var total float64
	matched := 0
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pg, err := strconv.Atoi(fields[0])
		if err != nil || pg != pgid {
			continue
		}
		secs, err := parseCPUTime(strings.Join(fields[1:], " "))
		if err != nil {
			return 0, 0, err
		}
		total += secs
		matched++
	}
	return total, matched, nil
}

// parseCPUTime accepts "MM:SS.hh", "HH:MM:SS.hh" or "D-HH:MM:SS.hh".
//
// Every component's parse error is surfaced. A malformed ps sample must
// register as a failed sample at the caller, which skips the tick and retries
// — never as a silent 0.0 that would keep the CPU limit from ever firing.
func parseCPUTime(s string) (float64, error) {
	if s == "" {
		return 0, fmt.Errorf("watchdog: empty cpu time")
	}
	days := 0.0
	if i := strings.Index(s, "-"); i > 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, fmt.Errorf("watchdog: bad cpu time days in %q: %w", s, err)
		}
		days = float64(d)
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m float64
	var secStr string
	var err error
	switch len(parts) {
	case 2:
		if m, err = strconv.ParseFloat(parts[0], 64); err != nil {
			return 0, fmt.Errorf("watchdog: bad cpu time minutes in %q: %w", s, err)
		}
		secStr = parts[1]
	case 3:
		if h, err = strconv.ParseFloat(parts[0], 64); err != nil {
			return 0, fmt.Errorf("watchdog: bad cpu time hours in %q: %w", s, err)
		}
		if m, err = strconv.ParseFloat(parts[1], 64); err != nil {
			return 0, fmt.Errorf("watchdog: bad cpu time minutes in %q: %w", s, err)
		}
		secStr = parts[2]
	default:
		return 0, fmt.Errorf("watchdog: unrecognized cpu time %q", s)
	}
	sec, err := strconv.ParseFloat(secStr, 64)
	if err != nil {
		return 0, fmt.Errorf("watchdog: bad cpu time seconds in %q: %w", s, err)
	}
	return days*86400 + h*3600 + m*60 + sec, nil
}
