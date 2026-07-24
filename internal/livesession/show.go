package livesession

import (
	"context"

	"github.com/luthermonson/shipmates/internal/attach"
	"github.com/luthermonson/shipmates/internal/turninput"
)

// ShowResult reports how a `show` landed on a live session, so the caller
// can tell the operator whether the file went into the turn that was
// already running or started a new one.
type ShowResult struct {
	ControlResult
	// Injected is true when the attachment was folded into an already
	// running turn rather than starting a new one.
	Injected bool `json:"injected"`
	// Notes are operator-facing one-liners about what happened to each
	// attachment (attached natively, inlined, truncated, referenced).
	Notes []string `json:"notes,omitempty"`
}

// ShowAttachment delivers validated attachments to a persona's live
// session. It captures the session's exact (session, thread, turn) tuple
// under the session lock and hands that same tuple to the delivery path, so
// it can never target a turn other than the one it observed: a turn that
// ends in between yields a StaleTarget refusal rather than a message
// delivered to the wrong turn.
//
// A running turn takes the attachment mid-flight; an idle session starts a
// new turn carrying it. Everything else is refused with the state's code so
// the caller can fall back to a one-shot delivery.
//
// How images travel depends on the backend: a transport that can carry an
// attachment mid-turn (the runtime-backed path — claude) gets them
// natively, while the codex app-server steer, which shipmates only sends as
// text, gets them as project-relative path references. Text is inlined
// either way and binary files are always referenced by path.
func (m *Manager) ShowAttachment(ctx context.Context, persona, caption string, files []turninput.FileDescriptorV1) (ShowResult, error) {
	s, err := m.Session(persona)
	if err != nil {
		return ShowResult{}, err
	}
	s.mu.Lock()
	if s.approval != nil {
		s.mu.Unlock()
		return ShowResult{}, failure(ApprovalPending)
	}
	snap := s.snapshotLocked()
	state := s.state
	a := s.adapter
	s.mu.Unlock()

	switch state {
	case Idle:
		plan, err := attach.Render(caption, files, attach.Native)
		if err != nil {
			return ShowResult{}, err
		}
		res, err := m.StartNextTurnInput(ctx, persona, snap.SessionID, snap.ThreadID, plan.Text, plan.Images)
		if err != nil {
			return ShowResult{}, err
		}
		return ShowResult{ControlResult: res, Notes: plan.Notes}, nil
	case Working:
		mode := attach.Reference
		if _, ok := a.(attachmentSteerer); ok {
			mode = attach.Native
		}
		plan, err := attach.Render(caption, files, mode)
		if err != nil {
			return ShowResult{}, err
		}
		res, err := m.TellInput(ctx, persona, snap.SessionID, snap.ThreadID, snap.TurnID, plan.Text, plan.Images)
		if err != nil {
			return ShowResult{}, err
		}
		return ShowResult{ControlResult: res, Injected: true, Notes: plan.Notes}, nil
	case Starting:
		return ShowResult{}, failure(StartingCode)
	case Steering, Interrupting:
		return ShowResult{}, failure(Busy)
	case Stopped:
		return ShowResult{}, failure(StoppedCode)
	case Failed:
		return ShowResult{}, failure(FailedCode)
	}
	return ShowResult{}, failure(Internal)
}
