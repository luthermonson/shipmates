// Package tracker is the seam between the voyage executor and task tracking.
//
// The interface is defined from what sail actually consumes — create a task
// record, mirror start/complete/block transitions with a bounded summary,
// record dependency edges, and read a bounded record back for prompt
// injection — not from what any particular backend happens to offer.
//
// Two backends implement it:
//
//   - markdown (the default): plain, human-readable files under
//     .shipmates/voyage/. No external dependency; the voyage's task state is
//     readable in a PR diff.
//   - beads: the bounded adapter to the external bd CLI, for projects that
//     installed bd and initialized a Beads workspace.
//
// Durability and integrity do NOT live here. The plan-hash-keyed voyage
// state, the hash-chained attempt ledger, and every fingerprint check are
// internal/voyage and internal/recovery concerns; a tracker only mirrors task
// state for humans and crew context, and sail never trusts it as evidence.
package tracker

import "context"

// Task is a tracker-agnostic task record request.
type Task struct {
	Title       string
	Description string
	Assignee    string
	ExternalRef string
	Labels      []string
}

// Tracker mirrors voyage task lifecycle into a tracking backend.
type Tracker interface {
	// Name identifies the backend ("markdown" or "beads") for display.
	Name() string
	// CreateTask records a new task and returns its opaque id. Recreating a
	// task with the same ExternalRef must be idempotent or fail loudly; it
	// must never silently mint a duplicate.
	CreateTask(ctx context.Context, t Task) (string, error)
	// AddDependency records that issue depends on dependency. It fails when
	// either id does not resolve (matching real bd behavior, which sail's
	// inherited-prerequisite handling relies on).
	AddDependency(ctx context.Context, id, dependsOn string) error
	// Start marks the task in progress for a persona.
	Start(ctx context.Context, id, persona string) error
	// Complete records the crew summary and closes the task.
	Complete(ctx context.Context, id, summary string) error
	// Block records the reason and marks the task blocked.
	Block(ctx context.Context, id, reason string) error
	// Show returns a bounded, human-readable record for prompt injection.
	// Failures return "" — prompt context is best-effort by design.
	Show(ctx context.Context, id string) string
	// Prime returns bounded backend workflow guidance for prompt injection,
	// or "".
	Prime(ctx context.Context) string
	// TaskGuidance returns the backend-specific instruction block telling a
	// crew turn how to consult and annotate its task record.
	TaskGuidance(id string) string
}
