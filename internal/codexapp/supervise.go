package codexapp

import (
	"context"
	"os/exec"
	"time"
)

// This file declares the process-supervision seam the app-server child is
// spawned through. It is expressed in stdlib terms only — no shipmates
// imports — for the reason stated in the package doc: internal/codexapp must
// build, vet and test on its own, while the shipmates process-containment
// package (internal/runtime/containment) is owned and evolved elsewhere.
//
// It is the same seam internal/runtime/claude uses, and deliberately so:
// codex and claude now get their containment from one portable Go
// implementation instead of two unrelated mechanisms. Binding the two is a
// short adapter the runtime layer owns — see
// internal/runtime/codex/containment_bridge.go.
//
// Limits do not appear here: they are containment policy, bound by whoever
// constructs the Supervisor, not something the transport forwards.

// Supervisor starts the app-server child under whatever containment policy
// the operator configured and hands back a Handle for observing and tearing
// down the resulting process tree.
//
// Start must attach its containment (process group, job object) and then
// start the process. It takes over the process lifecycle completely: the
// implementation calls cmd.Start and, on success, cmd.Wait, and the adapter
// calls neither. The caller has already wired cmd.Stdin/Stdout/Stderr and
// Start must not replace them.
type Supervisor interface {
	Start(cmd *exec.Cmd) (Handle, error)

	// Bounded reports whether Start actually applies containment — operator
	// limits and process-tree teardown. It exists so the runtime layer can
	// report Caps.Containment truthfully rather than inferring it from the
	// mere presence of a supervisor. A supervisor that only calls cmd.Start
	// must return false.
	Bounded() bool
}

// Handle is the transport's grip on the live app-server process tree.
type Handle interface {
	// Pid of the top-level process. On a supervisor that manages a tree this
	// is the tree root.
	Pid() int

	// Done fires at most once, with the terminal event describing why the
	// process ended, and is then closed. Consumers must tolerate a closed,
	// empty channel: a Handle whose event was already consumed reports
	// ok == false, which means "died, cause unknown".
	Done() <-chan Terminal

	// Close terminates the process AND its tree, then waits for it to exit.
	// Idempotent.
	//
	// ctx bounds the wait. A supervisor is expected to escalate from a
	// cooperative signal to a hard kill; an already-expired ctx asks it to
	// skip the cooperative phase, which is what ForceKill wants after
	// Adapter.Close has already spent the grace period itself.
	Close(ctx context.Context) error
}

// Terminal is the single terminal event a Handle emits.
type Terminal struct {
	// Reason is a stable machine-readable label — "exited", "requested",
	// "memory_limit", "cpu_limit", ... The transport does not interpret it;
	// it is carried so a supervisor-initiated kill is diagnosable.
	Reason string
	// Detail is human-readable context (exit status, the RSS reading that
	// breached the limit, the wait error).
	Detail string
	// ExitCode is the child's exit status, or -1 when it was killed before
	// setting one.
	ExitCode int
	// At records when the process ended.
	At time.Time
}
