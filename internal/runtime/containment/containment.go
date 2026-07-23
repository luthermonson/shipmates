// Package containment is the shipmates process-containment abstraction —
// how we bound a persona/turn subprocess's memory + CPU + tree-kill
// semantics. Multiple implementations live under this package; commands
// consume the Watcher interface only.
//
// Implementations:
//
//   - watchdog: cross-platform Go RSS watchdog + native process-tree kill
//     (Unix process groups, Windows Job Objects, macOS process groups).
//     Default for shipmates; zero privileges, zero install step.
//   - cgroup: Linux-only opt-in that delegates to the shipmates cgroup
//     launcher + codexapp.ExecutionContainment. Kernel-enforced, more
//     rigorous, requires `shipmates install` and delegated cgroup slice.
//   - none: no bounds, run process as-is. Explicit escape hatch for
//     debugging; not the default.
//
// The interface is deliberately minimal. All limit-relevant configuration
// travels through Limits so a factory can construct any Watcher from
// operator config uniformly.
package containment

import (
	"context"
	"os/exec"
	"time"
)

// Watcher wraps a subprocess with containment policy — start it, keep it
// bounded, tear it down on demand or on breach.
type Watcher interface {
	// Kind returns the containment mode name: "watchdog" | "cgroup" | "none".
	Kind() string

	// Start launches cmd under this watcher's containment. Caller keeps
	// ownership of cmd for stdin/stdout/stderr wiring; Start attaches the
	// containment side (process group, cgroup delegation, job object)
	// before Cmd.Start runs.
	//
	// The returned Handle Live()s until either the caller Closes it or the
	// watcher terminates the process for policy reasons (memory breach,
	// CPU quota exceeded, etc.). Consumers read Done() for the terminal
	// event.
	Start(cmd *exec.Cmd, limits Limits) (Handle, error)
}

// Handle is the caller's grip on a running watched process.
type Handle interface {
	// Pid of the top-level process. On watchers that manage a tree, this
	// is the tree root.
	Pid() int

	// Done fires exactly once when the process (and its tree) has exited,
	// either naturally or because the watcher killed it. Callers read it
	// to learn why the process ended.
	Done() <-chan Event

	// Close cooperatively terminates the process and its tree. Idempotent.
	// Uses ctx for the "wait for graceful exit before hard kill" deadline;
	// with a zero ctx or already-past deadline it hard-kills immediately.
	Close(ctx context.Context) error
}

// Event is the terminal signal a Handle emits on Done().
type Event struct {
	// Reason is a stable machine-readable label for what happened.
	Reason Reason
	// Detail is human-readable context (last RSS reading, exit code, etc.).
	Detail string
	// ExitCode is the child's exit status, or -1 if killed before setting one.
	ExitCode int
	// At records when the terminal event occurred.
	At time.Time
}

// Reason enumerates the ways a watched process can end.
type Reason string

const (
	// ReasonExited is a normal or error exit the child chose itself.
	ReasonExited Reason = "exited"
	// ReasonMemoryLimit means the watcher killed the process because RSS
	// exceeded Limits.MaxRSSBytes.
	ReasonMemoryLimit Reason = "memory_limit"
	// ReasonCPULimit means the watcher killed for CPU-time overrun.
	ReasonCPULimit Reason = "cpu_limit"
	// ReasonRequested means Close(ctx) initiated the shutdown.
	ReasonRequested Reason = "requested"
	// ReasonStartFailed means Start couldn't bring the process up.
	ReasonStartFailed Reason = "start_failed"
	// ReasonUnknown is a defensive default; consumers should treat it as
	// termination but log at higher severity.
	ReasonUnknown Reason = "unknown"
)

// Limits are the per-launch containment budget. Zero fields mean unlimited
// for that dimension.
type Limits struct {
	// MaxRSSBytes caps resident-set-size at the moment the watcher samples
	// it. 0 = uncapped. Watchdog polls at Limits.PollInterval; cgroup
	// enforces synchronously via memory.max.
	MaxRSSBytes int64

	// MaxCPUSeconds caps wall-clock CPU time (user + system) accumulated
	// by the process tree. 0 = uncapped.
	MaxCPUSeconds float64

	// PollInterval controls how often watchdog samples RSS/CPU. Ignored by
	// cgroup (kernel-driven). Defaults to 500ms when zero.
	PollInterval time.Duration

	// GracefulTimeout is the deadline for cooperative shutdown before the
	// watcher escalates to SIGKILL / TerminateJobObject. Defaults to 2s.
	GracefulTimeout time.Duration
}

// EffectivePollInterval returns Limits.PollInterval or the default.
func (l Limits) EffectivePollInterval() time.Duration {
	if l.PollInterval > 0 {
		return l.PollInterval
	}
	return 500 * time.Millisecond
}

// EffectiveGracefulTimeout returns Limits.GracefulTimeout or the default.
func (l Limits) EffectiveGracefulTimeout() time.Duration {
	if l.GracefulTimeout > 0 {
		return l.GracefulTimeout
	}
	return 2 * time.Second
}
