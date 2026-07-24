package livesession

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/luthermonson/shipmates/internal/attach"
	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
)

// runtimeBackend adapts a runtime.Runtime onto the live-session Backend
// seam, so a live session can run on claude exactly the way it runs on
// codex.
//
// Identity: the runtime's session id occupies livesession's thread slot and
// the runtime's turn id occupies the turn slot, so the exact-turn targeting
// that protects tell/interrupt from steering the wrong turn is preserved
// verbatim — a mismatched tuple is still a StaleTarget refusal, not a
// redirect.
//
// Capability gaps are surfaced, never faked. If the runtime does not report
// Steer, Interrupt or Attachments, the corresponding call returns a
// runtime-scoped error naming the runtime and the missing feature; nothing
// silently no-ops.
type runtimeBackend struct {
	rt         runtime.Runtime
	caps       runtime.Caps
	persona    string
	projectDir string

	events chan codexapp.Event

	mu sync.Mutex
	// sessionID is the runtime session (livesession's "thread").
	sessionID string
	// closing suppresses the terminal fault that a requested shutdown would
	// otherwise raise.
	closing bool
	// fanDone guards the translator goroutine's single start/stop.
	fanOnce  sync.Once
	stopFan  chan struct{}
	closeOne sync.Once
}

func newRuntimeBackend(rt runtime.Runtime, persona, projectDir string) *runtimeBackend {
	return &runtimeBackend{
		rt:         rt,
		caps:       rt.Capabilities(),
		persona:    persona,
		projectDir: projectDir,
		events:     make(chan codexapp.Event, 256),
		stopFan:    make(chan struct{}),
	}
}

func (b *runtimeBackend) unsupported(feature string) error {
	return fmt.Errorf("live session on runtime %s: %w", b.rt.Name(), &runtime.ErrUnsupported{Runtime: b.rt.Name(), Feature: feature})
}

func (b *runtimeBackend) spec(opts codexapp.ThreadOptions) runtime.SessionSpec {
	dir := opts.WorkingDirectory
	if dir == "" {
		dir = b.projectDir
	}
	return runtime.SessionSpec{Persona: b.persona, ProjectDir: dir, WorkingDir: dir}
}

// StartThread mints a fresh runtime session.
func (b *runtimeBackend) StartThread(ctx context.Context, opts codexapp.ThreadOptions) (codexapp.Thread, error) {
	s, err := b.rt.StartSession(ctx, b.spec(opts))
	if err != nil {
		return codexapp.Thread{}, err
	}
	return b.adopt(s.ID())
}

// ResumeThread reattaches to a runtime session recorded in the persona's
// live continuity marker.
func (b *runtimeBackend) ResumeThread(ctx context.Context, threadID string, opts codexapp.ThreadOptions) (codexapp.Thread, error) {
	s, err := b.rt.ResumeSession(ctx, threadID, b.spec(opts))
	if err != nil {
		return codexapp.Thread{}, err
	}
	if s.ID() != threadID {
		return codexapp.Thread{}, fmt.Errorf("live session on runtime %s: resume returned session %q, want %q", b.rt.Name(), s.ID(), threadID)
	}
	return b.adopt(s.ID())
}

func (b *runtimeBackend) adopt(id string) (codexapp.Thread, error) {
	b.mu.Lock()
	b.sessionID = id
	b.mu.Unlock()
	b.fanOnce.Do(func() { go b.fanout() })
	return codexapp.Thread{ID: id}, nil
}

// StartTurn sends one turn on the session. Image descriptors are
// materialized into runtime attachments here — immediately before dispatch
// — so each one is revalidated against the filesystem before its bytes
// leave the process.
func (b *runtimeBackend) StartTurn(ctx context.Context, threadID string, in codexapp.TurnInput) (codexapp.Turn, error) {
	input, err := b.turnInput(in)
	if err != nil {
		return codexapp.Turn{}, err
	}
	t, err := b.rt.SendTurn(ctx, threadID, input)
	if err != nil {
		return codexapp.Turn{}, err
	}
	return codexapp.Turn{ID: t.ID()}, nil
}

func (b *runtimeBackend) turnInput(in codexapp.TurnInput) (runtime.TurnInput, error) {
	if len(in.Images) == 0 {
		return runtime.TurnInput{Text: in.Text}, nil
	}
	if !b.caps.Attachments {
		return runtime.TurnInput{}, b.unsupported("attachments")
	}
	atts, err := attach.RuntimeAttachments(in.Images)
	if err != nil {
		return runtime.TurnInput{}, err
	}
	return runtime.TurnInput{Text: in.Text, Attachments: atts}, nil
}

// SteerTurn injects a mid-turn instruction into the exact active turn.
func (b *runtimeBackend) SteerTurn(ctx context.Context, threadID, turnID, text string) error {
	if !b.caps.Steer {
		return b.unsupported("steer")
	}
	return b.rt.SteerTurn(ctx, threadID, turnID, text)
}

// SteerTurnInput injects a mid-turn instruction that carries attachments.
// Runtimes that cannot take attachments mid-turn are refused rather than
// silently degraded to text.
func (b *runtimeBackend) SteerTurnInput(ctx context.Context, threadID, turnID string, in codexapp.TurnInput) error {
	if !b.caps.Steer {
		return b.unsupported("steer")
	}
	input, err := b.turnInput(in)
	if err != nil {
		return err
	}
	if len(input.Attachments) == 0 {
		return b.rt.SteerTurn(ctx, threadID, turnID, input.Text)
	}
	steerer, ok := b.rt.(interface {
		SteerTurnInput(context.Context, string, string, runtime.TurnInput) error
	})
	if !ok {
		return b.unsupported("steer with attachments")
	}
	return steerer.SteerTurnInput(ctx, threadID, turnID, input)
}

// InterruptTurn cancels the exact active turn.
func (b *runtimeBackend) InterruptTurn(ctx context.Context, threadID, turnID string) error {
	if !b.caps.Interrupt {
		return b.unsupported("interrupt")
	}
	return b.rt.InterruptTurn(ctx, threadID, turnID)
}

// ResolveApproval forwards an approval decision. Runtimes without an
// approval protocol return their own ErrUnsupported, which propagates
// unchanged so the mediation layer fails closed instead of pretending the
// answer landed.
func (b *runtimeBackend) ResolveApproval(ctx context.Context, r codexapp.ApprovalResponse, d codexapp.ApprovalDecision) (bool, error) {
	decision := runtime.ApprovalDecision{Allow: d == codexapp.AllowOnce, AllowFor: "once"}
	return b.rt.ResolveApproval(ctx, runtime.ApprovalResponse{
		ID:        r.RequestID,
		SessionID: r.ThreadID,
		TurnID:    r.TurnID,
		Kind:      "tool_use",
	}, decision)
}

func (b *runtimeBackend) Events() <-chan codexapp.Event { return b.events }

// ProcessGroupHandle returns nil: runtime-backed sessions are torn down via
// the runtime's own containment handle in Close, not by signalling a
// process group shipmates does not own.
func (b *runtimeBackend) ProcessGroupHandle() codexapp.ProcessGroupHandle { return nil }

// Close ends the runtime session and shuts the runtime down. Idempotent.
func (b *runtimeBackend) Close(ctx context.Context) error {
	var err error
	b.closeOne.Do(func() {
		b.mu.Lock()
		b.closing = true
		id := b.sessionID
		b.mu.Unlock()
		if id != "" {
			_ = b.rt.CloseSession(ctx, id)
		}
		err = b.rt.Close(ctx)
		close(b.stopFan)
	})
	return err
}

// fanout translates the runtime's normalized events into the narrow
// lifecycle events the session owner consumes.
//
// The session owner treats any event whose thread/turn tuple does not match
// the live one as a protocol violation and fails the session, so anything
// that cannot be attributed to the active turn is dropped here rather than
// forwarded.
func (b *runtimeBackend) fanout() {
	defer close(b.events)
	src := b.rt.Events()
	for {
		select {
		case <-b.stopFan:
			return
		case ev, ok := <-src:
			if !ok {
				b.emitTerminal(codexapp.UnexpectedEOF)
				return
			}
			out, forward := b.translate(ev)
			if !forward {
				continue
			}
			select {
			case b.events <- out:
			case <-b.stopFan:
				return
			}
		}
	}
}

func (b *runtimeBackend) emitTerminal(code codexapp.Code) {
	b.mu.Lock()
	closing := b.closing
	b.mu.Unlock()
	if closing {
		return
	}
	select {
	case b.events <- codexapp.Event{Kind: codexapp.AdapterFault, Code: code}:
	case <-b.stopFan:
	}
}

// translate maps one runtime event onto the codex-shaped lifecycle event.
// The bool reports whether it should be forwarded at all.
func (b *runtimeBackend) translate(ev runtime.Event) (codexapp.Event, bool) {
	out := codexapp.Event{ThreadID: ev.SessionID, TurnID: ev.TurnID}
	switch ev.Kind {
	case runtime.KindText:
		text, _ := ev.Payload.(string)
		if text == "" || ev.TurnID == "" {
			return out, false
		}
		out.Kind, out.Text = codexapp.AgentMessage, text
	case runtime.KindToolCall:
		if ev.TurnID == "" {
			return out, false
		}
		out.Kind = codexapp.Activity
		out.Category, out.Detail = toolActivity(ev.Payload)
	case runtime.KindTurnDone:
		if ev.TurnID == "" {
			return out, false
		}
		out.Kind = codexapp.TurnCompleted
	case runtime.KindError:
		if ev.TurnID == "" {
			// No turn to attribute the failure to: this is a transport
			// fault, which the owner treats as terminal.
			b.emitTerminal(codexapp.ChildExit)
			return out, false
		}
		out.Kind = codexapp.TurnFailed
	case runtime.KindSessionClosed:
		b.emitTerminal(codexapp.ChildExit)
		return out, false
	case runtime.KindApprovalNeeded:
		// No runtime on this path issues approval requests yet; forwarding
		// one without the policy binding the mediator requires would fail
		// the session, so it is dropped with a loud log instead.
		slog.Warn("live session: dropping approval request from runtime without approval mediation", "runtime", b.rt.Name(), "session", ev.SessionID, "turn", ev.TurnID)
		return out, false
	default:
		// KindToolResult and KindBackend carry no lifecycle meaning.
		return out, false
	}
	return out, true
}

// toolActivity maps a tool call onto the activity taxonomy the feed uses.
func toolActivity(payload any) (string, string) {
	tc, ok := payload.(claude.ToolCall)
	if !ok {
		return "other", ""
	}
	switch tc.Name {
	case "Bash", "PowerShell":
		return "command", tc.Name
	case "Edit", "Write", "NotebookEdit":
		return "file_change", tc.Name
	case "WebSearch", "WebFetch":
		return "web_search", tc.Name
	case "":
		return "other", ""
	default:
		return "other", tc.Name
	}
}

// runtimeBackendError reports whether err came from a runtime capability
// gap, so callers can distinguish "this runtime cannot do that" from a
// transport failure.
func runtimeBackendError(err error) bool {
	var unsupported *runtime.ErrUnsupported
	return errors.As(err, &unsupported)
}

var _ Backend = (*runtimeBackend)(nil)
var _ attachmentSteerer = (*runtimeBackend)(nil)
