package livesession

import (
	"context"

	"github.com/luthermonson/shipmates/internal/codexapp"
)

// Backend is the transport seam the live-session owner talks to. It is the
// exact surface the state machine needs and nothing more: establish a
// thread, run one turn at a time on it, steer or interrupt that exact turn,
// mediate approvals, and stream lifecycle events.
//
// Two implementations exist:
//
//   - *codexapp.Adapter satisfies it structurally, so the codex path is
//     byte-for-byte the code it always was.
//   - runtimeBackend adapts any runtime.Runtime (claude today) onto it,
//     which is what makes `live`, `tell`, `feed`, `interrupt` and `show`
//     work on a non-codex runtime.
//
// The vocabulary stays codexapp's on purpose. Those types are already
// backend-neutral by design — Thread and Turn are opaque identities, Event
// is a narrow lifecycle signal with no protocol payload — and reusing them
// keeps the codex path's behavior, and its test suite, untouched.
//
// Identity mapping: codex is thread+turn, claude is session+turn. Both land
// on livesession's existing (sessionID, threadID, turnID) tuple, with the
// runtime session id taking the thread slot. Exact-turn targeting on tell
// and interrupt therefore keeps working unchanged on both.
type Backend interface {
	StartThread(ctx context.Context, opts codexapp.ThreadOptions) (codexapp.Thread, error)
	ResumeThread(ctx context.Context, threadID string, opts codexapp.ThreadOptions) (codexapp.Thread, error)
	StartTurn(ctx context.Context, threadID string, in codexapp.TurnInput) (codexapp.Turn, error)
	SteerTurn(ctx context.Context, threadID, turnID, text string) error
	InterruptTurn(ctx context.Context, threadID, turnID string) error
	ResolveApproval(ctx context.Context, r codexapp.ApprovalResponse, d codexapp.ApprovalDecision) (bool, error)
	Events() <-chan codexapp.Event
	ProcessGroupHandle() codexapp.ProcessGroupHandle
	Close(ctx context.Context) error
}

// attachmentSteerer is the optional half of Backend for injecting an
// attachment into an already-running turn. Backends that can carry
// attachments mid-turn implement it; callers that need it type-assert and
// fall back to a plain text steer otherwise.
type attachmentSteerer interface {
	SteerTurnInput(ctx context.Context, threadID, turnID string, in codexapp.TurnInput) error
}

// Compile-time proof that the codex adapter still satisfies the seam.
var _ Backend = (*codexapp.Adapter)(nil)
