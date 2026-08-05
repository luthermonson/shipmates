//go:build unix && !linux && !darwin

package watchdog

import (
	"fmt"
	"os/exec"
	"syscall"

	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// This is the fallback for Unixes that are neither Linux nor macOS (the BSDs).
// Process-group teardown works everywhere on Unix, so tree-kill is fully
// supported; RSS and CPU sampling is not implemented, because there is no
// /proc/<pid>/statm and no reason to guess at a ps output format nobody here
// can test.
//
// Rather than let the sampler skip every tick — which would present a limit
// as enforced while enforcing nothing — prepare refuses a bounded launch
// outright. See the "Caps must be truthful" note in internal/runtime.

// prepare puts the child in its own process group, and refuses limits this
// platform cannot enforce.
func prepare(cmd *exec.Cmd, limits containment.Limits) error {
	if limits.Bounded() {
		return fmt.Errorf("watchdog: memory/CPU limits are not implemented on this platform; use containment mode \"none\" or leave the limits unset")
	}
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

// sampleRSS is unreachable while prepare refuses bounded launches; it exists
// so the shared sampler compiles.
func sampleRSS(int) (int64, error) {
	return 0, fmt.Errorf("watchdog: RSS sampling not implemented on this platform")
}

// sampleCPUSeconds is unreachable while prepare refuses bounded launches; it
// exists so the shared sampler compiles.
func sampleCPUSeconds(int) (float64, error) {
	return 0, fmt.Errorf("watchdog: CPU sampling not implemented on this platform")
}
