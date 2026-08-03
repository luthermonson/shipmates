package claude

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"
)

// This file declares the process-supervision seam this package spawns session
// processes through. It is expressed in stdlib terms only — no shipmates
// imports — for one reason: internal/runtime/claude must build, vet and test
// on its own, and the shipmates process-containment package
// (internal/runtime/containment) is owned and evolved elsewhere.
//
// Binding the two is a short adapter the runtime factory owns:
//
//	w := watchdog.New()
//	rt := claude.New(claude.Config{Supervisor: claude.SupervisorFunc(
//		func(cmd *exec.Cmd) (claude.Handle, error) {
//			h, err := w.Start(cmd, limits)
//			if err != nil {
//				return nil, err
//			}
//			out := make(chan claude.Terminal, 1)
//			go func() {
//				defer close(out)
//				if ev, ok := <-h.Done(); ok {
//					out <- claude.Terminal{
//						Reason:   string(ev.Reason),
//						Detail:   ev.Detail,
//						ExitCode: ev.ExitCode,
//						At:       ev.At,
//					}
//				}
//			}()
//			return claude.NewHandle(h.Pid(), out, h.Close), nil
//		})})
//
// Limits deliberately do not appear here: they are containment policy, bound
// by whoever constructs the Supervisor, not something a runtime forwards.

// Supervisor starts one session subprocess under whatever containment policy
// the operator configured and hands back a Handle for observing and tearing
// down the resulting process tree.
//
// Start must attach its containment (process group, job object) and then start
// the process; the caller has already wired cmd.Stdin/Stdout/Stderr and must
// not have them replaced.
type Supervisor interface {
	Start(cmd *exec.Cmd) (Handle, error)
}

// SupervisorFunc adapts a plain function to Supervisor.
type SupervisorFunc func(cmd *exec.Cmd) (Handle, error)

// Start implements Supervisor.
func (f SupervisorFunc) Start(cmd *exec.Cmd) (Handle, error) { return f(cmd) }

// Handle is the runtime's grip on a live session process.
type Handle interface {
	// Pid of the top-level process. On a supervisor that manages a tree this
	// is the tree root.
	Pid() int

	// Done fires at most once, with the terminal event describing why the
	// process ended, and is then closed. Consumers must tolerate a closed,
	// empty channel: a Handle whose event was already consumed reports
	// ok == false, which the read loop treats as "died, cause unknown".
	Done() <-chan Terminal

	// Close terminates the process (and its tree, where the supervisor can)
	// and waits for the terminal event or ctx, whichever comes first.
	// Idempotent.
	Close(ctx context.Context) error
}

// Terminal is the single terminal event a Handle emits. Detail is the
// diagnosis slot — an exit code alone never explains WHY claude refused to
// run, so the child's captured stderr tail is appended to it before the event
// reaches a consumer.
type Terminal struct {
	// Reason is a stable machine-readable label. The constants below are the
	// ones this package produces; a containment-backed Supervisor may pass
	// through its own richer set (e.g. "memory_limit", "cpu_limit").
	Reason string
	// Detail is human-readable context (exit status, last RSS reading,
	// captured stderr).
	Detail string
	// ExitCode is the child's exit status, or -1 when it was killed before
	// setting one.
	ExitCode int
	// At records when the process ended.
	At time.Time
}

const (
	// ReasonExited is a normal or error exit the child chose itself.
	ReasonExited = "exited"
	// ReasonRequested means Close initiated the shutdown.
	ReasonRequested = "requested"
	// ReasonStartFailed means the process never came up.
	ReasonStartFailed = "start_failed"
	// ReasonUnknown is a defensive default; consumers should treat it as
	// termination but log at higher severity.
	ReasonUnknown = "unknown"
)

// NewHandle wraps an already-running process's three observable facts into a
// Handle. It exists so an adapter over a containment watcher is a handful of
// lines rather than a type declaration.
func NewHandle(pid int, done <-chan Terminal, closeFn func(context.Context) error) Handle {
	if closeFn == nil {
		closeFn = func(context.Context) error { return nil }
	}
	return &adaptedHandle{pid: pid, done: done, closeFn: closeFn}
}

type adaptedHandle struct {
	pid     int
	done    <-chan Terminal
	closeFn func(context.Context) error
}

func (h *adaptedHandle) Pid() int              { return h.pid }
func (h *adaptedHandle) Done() <-chan Terminal { return h.done }
func (h *adaptedHandle) Close(c context.Context) error {
	return h.closeFn(c)
}

// directSupervisor is the default: start the process as-is, apply no bounds.
// It is the explicit escape hatch rather than a hidden nil-Supervisor special
// case, and it is what the tests run under.
//
// Its Close kills the top-level process only. Tree semantics (Unix process
// groups, Windows job objects) belong to a containment-backed Supervisor —
// this one makes no claim it cannot keep on every platform.
type directSupervisor struct{}

func (directSupervisor) Start(cmd *exec.Cmd) (Handle, error) {
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("claude: start session process: %w", err)
	}
	h := &directHandle{cmd: cmd, done: make(chan Terminal, 1)}
	go h.wait()
	return h, nil
}

type directHandle struct {
	cmd  *exec.Cmd
	done chan Terminal
	once sync.Once
	// killed records that Close asked for the shutdown, so the terminal event
	// says "requested" rather than blaming the child for the exit we caused.
	killed atomicBool
}

func (h *directHandle) Pid() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *directHandle) Done() <-chan Terminal { return h.done }

func (h *directHandle) Close(ctx context.Context) error {
	h.killed.set()
	if h.cmd.Process != nil {
		// Best effort: an already-reaped process errors here, which is not a
		// failure of Close.
		_ = h.cmd.Process.Kill()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-h.done:
	case <-ctx.Done():
	}
	return nil
}

func (h *directHandle) wait() {
	err := h.cmd.Wait()
	ev := Terminal{Reason: ReasonExited, ExitCode: -1, At: time.Now()}
	if h.cmd.ProcessState != nil {
		ev.ExitCode = h.cmd.ProcessState.ExitCode()
	}
	if err != nil {
		ev.Detail = err.Error()
	}
	if h.killed.get() {
		ev.Reason = ReasonRequested
	}
	h.once.Do(func() {
		h.done <- ev
		close(h.done)
	})
}

// atomicBool is a mutex-guarded flag. sync/atomic would do, but the flag is
// written once and read once per process lifetime, so clarity wins.
type atomicBool struct {
	mu sync.Mutex
	v  bool
}

func (b *atomicBool) set() {
	b.mu.Lock()
	b.v = true
	b.mu.Unlock()
}

func (b *atomicBool) get() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.v
}
