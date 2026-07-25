package livesession

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/attach"
	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/policy"
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
	// turnPolicy is the immutable policy snapshot bound to the turn now in
	// flight, captured from codexapp.TurnInput.Policy on the way in. An
	// approval that arrives without one cannot be mediated and is denied.
	//
	// One entry is enough: every runtime on this seam is one-turn-at-a-time,
	// and it is installed BEFORE the turn is dispatched so an approval can
	// never beat its own snapshot into the fanout.
	turnPolicy *policy.Snapshot
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
	// Bind the turn's policy snapshot before dispatch: the runtime may emit
	// an approval the instant the turn starts, and an approval whose snapshot
	// is missing is refused rather than mediated.
	b.mu.Lock()
	b.turnPolicy = in.Policy
	b.mu.Unlock()
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

// ResolveApproval forwards the mediator's decision to the runtime. Only
// allow-once is ever granted: shipmates scopes approvals to the single call
// and never asks a runtime to remember one. Runtimes without an approval
// protocol return their own ErrUnsupported, which propagates unchanged so
// the mediation layer fails closed instead of pretending the answer landed.
func (b *runtimeBackend) ResolveApproval(ctx context.Context, r codexapp.ApprovalResponse, d codexapp.ApprovalDecision) (bool, error) {
	decision := runtime.ApprovalDecision{Allow: d == codexapp.AllowOnce, AllowFor: "once"}
	if !decision.Allow {
		decision.Rationale = "Denied by the shipmates operator or by project policy."
	}
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
		return b.translateApproval(ev)
	default:
		// KindToolResult and KindBackend carry no lifecycle meaning.
		return out, false
	}
	return out, true
}

// translateApproval maps a runtime approval request onto the same
// ApprovalRequested event the codex adapter emits, so the session owner's
// mediation — policy evaluation, the operator's 30s window, the audit feed,
// `open` and the dashboard — runs byte-for-byte the way it does on codex.
//
// It is the fail-closed boundary. An approval this backend cannot bind to
// an exact turn and policy snapshot is never forwarded (the owner would
// treat an unbindable tuple as a protocol violation and kill the session);
// instead it is denied straight back to the runtime, because a request left
// unanswered stalls the turn forever.
func (b *runtimeBackend) translateApproval(ev runtime.Event) (codexapp.Event, bool) {
	out := codexapp.Event{ThreadID: ev.SessionID, TurnID: ev.TurnID}
	req, ok := ev.Payload.(claude.ApprovalRequest)
	if !ok || req.RequestID == "" {
		slog.Warn("live session: unusable approval payload from runtime", "runtime", b.rt.Name(), "session", ev.SessionID, "turn", ev.TurnID)
		return out, false
	}
	b.mu.Lock()
	snapshot := b.turnPolicy
	b.mu.Unlock()

	command, commandOK := ApprovalCommandExact(req)
	if ev.TurnID == "" || snapshot == nil || !commandOK {
		reason := "no policy snapshot bound to the turn"
		if ev.TurnID == "" {
			reason = "approval carries no turn attribution"
		} else if !commandOK {
			reason = "approval request could not be normalized"
		}
		slog.Warn("live session: denying unmediatable approval",
			"runtime", b.rt.Name(), "session", ev.SessionID, "turn", ev.TurnID,
			"tool", req.ToolName, "reason", reason)
		b.denyApproval(ev.SessionID, ev.TurnID, req.RequestID, "shipmates could not mediate this request: "+reason)
		return out, false
	}

	explanation := policy.Evaluate(snapshot, policy.Request{Kind: policy.ProcessExec, CommandExact: command})
	out.Kind = codexapp.ApprovalRequested
	out.RequestClass = "approval"
	out.BackendRequestID = req.RequestID
	out.CommandExact = command
	out.PolicySnapshotID = snapshot.ID
	out.PolicyEffect = explanation.PolicyEffect
	out.MatchedRules = explanation.MatchedRules
	out.OmittedMatches = explanation.OmittedMatches
	return out, true
}

// denyApproval refuses a request the mediation path will not see. It runs
// off the fanout goroutine because writing to the runtime must never block
// event delivery.
func (b *runtimeBackend) denyApproval(sessionID, turnID, requestID, rationale string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
		defer cancel()
		if _, err := b.rt.ResolveApproval(ctx, runtime.ApprovalResponse{
			ID: requestID, SessionID: sessionID, TurnID: turnID, Kind: "tool_use",
		}, runtime.ApprovalDecision{Allow: false, AllowFor: "once", Rationale: rationale}); err != nil {
			slog.Warn("live session: could not deny unmediatable approval", "runtime", b.rt.Name(), "request", requestID, "error", err)
		}
	}()
}

// ApprovalCommandExact normalizes a runtime approval into the exact-match
// string shipmates policy rules are written against.
//
// Shell tools yield their command verbatim, which is precisely what the
// codex adapter passes for item/commandExecution/requestApproval — so one
// `process.exec` rule governs the same command on either runtime.
//
// Every other tool has no command line, so it is represented as
// `Tool(primary-argument)`. That keeps the request nameable in a policy
// file and in the audit feed without ever presenting tool arguments as a
// shell command. Such a descriptor matches no real command rule, so it
// falls through to `ask` and reaches the operator — fail-safe by default.
//
// The bool reports whether a usable descriptor exists at all; an oversized
// or control-character-bearing request produces none and is denied.
func ApprovalCommandExact(req claude.ApprovalRequest) (string, bool) {
	command, ok := req.Command()
	if !ok {
		tool := req.ToolName
		if tool == "" {
			return "", false
		}
		command = tool + "(" + approvalPrimaryArgument(req) + ")"
	}
	if command == "" || len(command) > maxApprovalCommandBytes || !utf8.ValidString(command) {
		return "", false
	}
	return command, true
}

// approvalPrimaryArgument picks the one input field that identifies what a
// non-shell tool is about to touch, in a fixed order so the descriptor is
// deterministic. Unknown tools contribute no argument rather than an
// arbitrary one.
func approvalPrimaryArgument(req claude.ApprovalRequest) string {
	var input map[string]json.RawMessage
	if json.Unmarshal(req.InputJSON, &input) != nil {
		return ""
	}
	for _, key := range []string{"file_path", "path", "notebook_path", "url", "pattern", "query", "command"} {
		raw, ok := input[key]
		if !ok {
			continue
		}
		var v string
		if json.Unmarshal(raw, &v) == nil && v != "" {
			return v
		}
	}
	return ""
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
