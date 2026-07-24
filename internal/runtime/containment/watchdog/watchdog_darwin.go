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

// prepare puts the child into its own process group (same as Linux). The
// limits argument is accepted for signature parity with Windows and is
// currently unused on macOS (memory/CPU enforced by the poll loop).
func prepare(cmd *exec.Cmd, _ containment.Limits) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

func attach(*exec.Cmd, containment.Limits) error { return nil }

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

// sampleRSS shells out to ps -o rss=. macOS has no /proc equivalent and no
// pure-Go path to task_info; ps is the pragmatic pure-Go answer. Cost is a
// fork per poll interval — acceptable at 500ms cadence but revisit if we
// tighten the interval.
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

// sampleCPUSeconds via ps -o time= (returns hh:mm:ss.hh format).
func sampleCPUSeconds(pid int) (float64, error) {
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return 0, err
	}
	return parseCPUTime(strings.TrimSpace(string(out)))
}

// parseCPUTime accepts "MM:SS.hh" or "HH:MM:SS.hh" or "D-HH:MM:SS.hh".
// Every component parse error is surfaced: a malformed ps sample must
// register as a failed sample at the caller (which skips it and retries
// next tick), never as a silent 0.0 that would keep the CPU limit from
// ever triggering.
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
	if len(parts) < 2 {
		return 0, fmt.Errorf("watchdog: unrecognized cpu time %q", s)
	}
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
