// Package containment is the shipmates process-containment abstraction —
// how a runtime's turn subprocess is bounded in memory and CPU, and how its
// whole process tree is torn down.
//
// Implementations:
//
//   - watchdog: cross-platform. Native process-tree teardown (Unix process
//     groups, Windows Job Objects) plus a polling RSS/CPU sampler. Zero
//     privileges, zero install step. This is the shipmates default.
//   - none: no bounds, run the process as-is. An explicit escape hatch for
//     debugging, and a first-class implementation so "containment off" does
//     not become a nil-Watcher special case in every caller.
//
// These two are the whole set. A "cgroup" mode was once named in the config
// schema for Linux deployments wanting kernel-enforced limits; it has been
// removed, along with the Linux-only launcher that backed it inside
// internal/codexapp. Naming it now is a hard error rather than a silent
// downgrade — see config.ErrCgroupModeRemoved.
//
// The reason is portability, not simplicity. Containment that works on one
// kernel is containment most operators do not have, and shipmates would rather
// give every platform the same bounds — sampled RSS/CPU plus real tree
// teardown — than give one platform better ones and the rest a footnote.
// Windows still gets kernel enforcement for free, because Job Objects express
// memory and process-count caps natively.
//
// The interface is deliberately minimal, and every limit travels through
// [Limits], so a factory can build any Watcher from operator config the same
// way.
//
// # Trust
//
// Containment posture is the operator's decision, taken from user config
// only. See internal/runtime/config for why a project checkout may not
// choose it.
package containment

import (
	"context"
	"os/exec"
	"time"
)

// Watcher wraps a subprocess with containment policy — start it, keep it
// bounded, tear it down on demand or on breach.
type Watcher interface {
	// Kind returns the containment mode name: "watchdog" | "none".
	Kind() string

	// Start launches cmd under this watcher's containment. The caller keeps
	// ownership of cmd for stdin/stdout/stderr wiring; Start attaches the
	// containment side (process group, job object) around Cmd.Start.
	//
	// Start takes over the process lifecycle: it calls cmd.Start and, on
	// success, cmd.Wait. Callers must not call either themselves, and must
	// read Done() for the terminal event instead of inspecting
	// cmd.ProcessState.
	Start(cmd *exec.Cmd, limits Limits) (Handle, error)
}

// Handle is the caller's grip on a running watched process.
type Handle interface {
	// Pid of the top-level process. On watchers that manage a tree, this is
	// the tree root.
	Pid() int

	// Done delivers exactly one Event — why the process ended — and is then
	// closed. It is safe to read after the event has been delivered; a
	// second read yields the zero Event because the channel is closed, so
	// callers that care should keep the first value.
	Done() <-chan Event

	// Close cooperatively terminates the process and its tree, then waits
	// for it to exit. Idempotent.
	//
	// ctx bounds the wait: with a deadline, the watcher escalates from a
	// cooperative signal to a hard kill at Limits.EffectiveGracefulTimeout
	// and gives up waiting when ctx expires. A nil or already-expired ctx
	// hard-kills promptly.
	Close(ctx context.Context) error
}

// Event is the terminal signal a Handle delivers on Done().
type Event struct {
	// Reason is a stable machine-readable label for what happened.
	Reason Reason
	// Detail is human-readable context (the RSS reading that breached the
	// limit, the wait error, etc.). May be empty.
	Detail string
	// ExitCode is the child's exit status, or -1 if it was killed before
	// setting one.
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
	// ReasonCPULimit means the watcher killed the process for CPU-time
	// overrun against Limits.MaxCPUSeconds.
	ReasonCPULimit Reason = "cpu_limit"
	// ReasonRequested means Close(ctx) initiated the shutdown.
	ReasonRequested Reason = "requested"
	// ReasonStartFailed means Start could not bring the process up. Start
	// returns an error in that case, so this appears only where a watcher
	// reports the failure through a Handle.
	ReasonStartFailed Reason = "start_failed"
	// ReasonUnknown is a defensive default; consumers should treat it as
	// termination but log it at higher severity.
	ReasonUnknown Reason = "unknown"
)

// Limits is the per-launch containment budget. A zero field means unlimited
// in that dimension — the default posture imposes no cap nobody asked for.
type Limits struct {
	// MaxRSSBytes caps resident set size. 0 = uncapped.
	//
	// The watchdog samples it every PollInterval, so enforcement lags a
	// spike by up to one interval. On Windows the same value is additionally
	// programmed as a Job Object memory cap, which the kernel enforces
	// synchronously with no polling gap.
	MaxRSSBytes int64

	// MaxCPUSeconds caps CPU time (user + system) accumulated by the
	// process. 0 = uncapped. Polled, on every platform.
	MaxCPUSeconds float64

	// PollInterval controls how often the watchdog samples RSS and CPU.
	// Defaults to 500ms when zero. See EffectivePollInterval.
	PollInterval time.Duration

	// GracefulTimeout is how long Close waits for a cooperative exit before
	// escalating to a hard kill. Defaults to 2s. See
	// EffectiveGracefulTimeout.
	GracefulTimeout time.Duration

	// MaxProcesses caps the number of active processes in the tree.
	// 0 = uncapped. Kernel-enforced on Windows only, via
	// JOB_OBJECT_LIMIT_ACTIVE_PROCESS; Linux and macOS ignore it today
	// (there is no polling equivalent worth having).
	MaxProcesses uint32
}

// DefaultPollInterval is the watchdog's sampling cadence when Limits leaves
// PollInterval zero.
const DefaultPollInterval = 500 * time.Millisecond

// DefaultGracefulTimeout is how long Close waits for a cooperative exit when
// Limits leaves GracefulTimeout zero.
const DefaultGracefulTimeout = 2 * time.Second

// EffectivePollInterval returns Limits.PollInterval or DefaultPollInterval.
func (l Limits) EffectivePollInterval() time.Duration {
	if l.PollInterval > 0 {
		return l.PollInterval
	}
	return DefaultPollInterval
}

// EffectiveGracefulTimeout returns Limits.GracefulTimeout or
// DefaultGracefulTimeout.
func (l Limits) EffectiveGracefulTimeout() time.Duration {
	if l.GracefulTimeout > 0 {
		return l.GracefulTimeout
	}
	return DefaultGracefulTimeout
}

// Bounded reports whether any resource cap is set. Watchers use it to skip
// starting a sampler that could never fire.
func (l Limits) Bounded() bool {
	return l.MaxRSSBytes > 0 || l.MaxCPUSeconds > 0
}
