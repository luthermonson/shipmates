// Package codex wraps internal/codexapp.Adapter to satisfy the
// runtime.Runtime interface. The adapter's method names and shapes are close
// to the interface already (StartThread → StartSession, StartTurn →
// SendTurn, etc.); this file is thin glue plus event normalization.
//
// Containment posture: codex self-contains inside codexapp.Adapter via
// codexapp.StartOptions.RequireExecutionContainment (kernel-enforced on Linux
// through cgroups). The runtime.Runtime layer does NOT double-wrap here. The
// claude runtime, by contrast, routes its exec.Cmd spawn through a containment
// watcher because there is no equivalent in-adapter enforcement.
//
// TODO(cgroup-watcher): if a future codexapp surface lets us intercept
// child-process spawns, route them through the shared containment watcher too so
// the two runtimes share one containment story.
package codex

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// Runtime is the codex-backed runtime.Runtime. Construct via New.
type Runtime struct {
	adapter *codexapp.Adapter
	caps    codexapp.Capabilities
	// contained records whether the transport was started under kernel-enforced
	// execution containment. Factory.Start fails outright when containment was
	// requested and could not be established, so a live Runtime with this set is
	// genuinely contained — which is what lets Caps.Containment be truthful
	// instead of hardcoded false.
	contained bool
	stream    chan runtime.Event
	stopFan   chan struct{}
	stopped   sync.Once

	sessMu   sync.Mutex
	sessions map[string]*session
}

// New starts a codex app-server and returns a runtime.Runtime backed by it.
// opts control the transport (working dir, containment, credential
// isolation, etc.); the caller provides them because the shipmates command
// layer knows the operator's project + policy context.
func New(ctx context.Context, opts codexapp.StartOptions) (*Runtime, error) {
	adapter, caps, err := codexapp.Factory{}.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	r := &Runtime{
		adapter:   adapter,
		caps:      caps,
		contained: opts.RequireExecutionContainment,
		stream:    make(chan runtime.Event, 64),
		stopFan:   make(chan struct{}),
		sessions:  map[string]*session{},
	}
	go r.fanout()
	return r, nil
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "codex" }

// Capabilities implements runtime.Runtime.
//
// Every field below is claimed only where this package actually does the work;
// two of them were previously overstated and the notes say so, because a
// capability report is the one thing callers cannot verify for themselves.
func (r *Runtime) Capabilities() runtime.Caps {
	return runtime.Caps{
		// The adapter forwards item/agentMessage/delta as partial AgentMessage
		// events, so output arrives during the turn rather than at the end.
		Streaming: true,
		Interrupt: r.caps.Interrupt,
		Steer:     r.caps.Steer,
		// Attachments is true because SendTurn now translates in.Attachments into
		// codexapp localImage items and StartTurn revalidates them on the way out.
		// It was previously reported true while SendTurn dropped the field on the
		// floor, so operators were told their screenshots had been sent when
		// nothing had been.
		Attachments: r.caps.LocalImage,
		Refusal:     r.caps.RequestRefusal,
		// Containment is kernel-enforced by the transport when the caller asked
		// for it; see the field comment on Runtime.contained.
		//
		// The grain is coarser than the interface's: containment is decided once
		// for the transport in New, not per session, so SessionSpec.ContainExec is
		// not consulted. When this is true every session is contained — including
		// one whose spec left ContainExec false, which over-delivers rather than
		// over-claims. When it is false, ContainExec cannot be honored at all and a
		// caller that set it is being ignored, exactly as the interface describes.
		Containment: r.contained,
		// The codex app-server transport has no per-session process environment
		// (codexapp.ThreadOptions cannot carry one), so SessionSpec.Environment is
		// unsupported — StartSession rejects it rather than dropping it.
		Environment: false,
		// Approvals is true because the transport opens threads with
		// approvalPolicy "on-request", holds each request open instead of
		// auto-answering it, and surfaces it as KindApprovalNeeded carrying the
		// runtime.ApprovalResponse that ResolveApproval expects back. An earlier
		// version reported true while the adapter cancelled every request on
		// arrival and omitted it from the stream, so no approval was ever
		// mediated and no operator was ever asked.
		Approvals: true,
	}
}

// Events implements runtime.Runtime.
func (r *Runtime) Events() <-chan runtime.Event { return r.stream }

// StartSession implements runtime.Runtime. SessionSpec.Environment is
// rejected rather than silently dropped: codexapp.ThreadOptions has no way
// to carry a per-session process environment through the app-server
// transport, and pretending otherwise would let callers believe an
// override took effect (Caps.Environment is false accordingly).
func (r *Runtime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	if len(spec.Environment) > 0 {
		return nil, &runtime.ErrUnsupported{Runtime: "codex", Feature: "SessionSpec.Environment"}
	}
	th, err := r.adapter.StartThread(ctx, threadOptsFromSpec(spec))
	if err != nil {
		return nil, err
	}
	return r.rememberSession(th.ID, spec), nil
}

// ResumeSession implements runtime.Runtime. Rejects SessionSpec.Environment
// for the same reason as StartSession.
func (r *Runtime) ResumeSession(ctx context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	if len(spec.Environment) > 0 {
		return nil, &runtime.ErrUnsupported{Runtime: "codex", Feature: "SessionSpec.Environment"}
	}
	th, err := r.adapter.ResumeThread(ctx, id, threadOptsFromSpec(spec))
	if err != nil {
		return nil, err
	}
	return r.rememberSession(th.ID, spec), nil
}

// CloseSession is a no-op at the transport layer; codex app-server sessions
// (threads) are closed implicitly when the adapter closes. Callers use this
// method to drop shipmates-side session bookkeeping.
func (r *Runtime) CloseSession(_ context.Context, id string) error {
	r.sessMu.Lock()
	delete(r.sessions, id)
	r.sessMu.Unlock()
	return nil
}

// SendTurn implements runtime.Runtime. Image attachments are translated into
// codex localImage items; the adapter revalidates each file immediately before
// the path goes on the wire, because the app-server opens the file itself.
func (r *Runtime) SendTurn(ctx context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	images, err := localImages(in.Attachments)
	if err != nil {
		return nil, err
	}
	t, err := r.adapter.StartTurn(ctx, sessionID, codexapp.TurnInput{Text: in.Text, Images: images})
	if err != nil {
		return nil, err
	}
	return &turn{id: t.ID, sessionID: sessionID, startedAt: time.Now()}, nil
}

// codexLocalImageMediaTypes are the formats codex accepts as a localImage item
// and this package can verify from magic bytes. Anything else is refused with a
// named error rather than forwarded and hoped for.
var codexLocalImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// localImages translates runtime attachments into codexapp descriptors.
//
// The caller has already validated and rooted these paths (see the
// runtime.Attachment doc), so this does not re-root them; it enforces only what
// the codex wire format itself requires and refuses everything it cannot carry.
// Attachment.Base64 is deliberately unused: codex reads the file from disk by
// path, so pre-read bytes have nowhere to go.
func localImages(attachments []runtime.Attachment) ([]codexapp.LocalImage, error) {
	if len(attachments) == 0 {
		return nil, nil
	}
	out := make([]codexapp.LocalImage, 0, len(attachments))
	for _, a := range attachments {
		name := a.DisplayPath
		if name == "" {
			name = "attachment"
		}
		if a.Kind != "" && a.Kind != "image" {
			return nil, fmt.Errorf("codex: attachment %s: kind %q cannot be sent to the app-server; only images are carried natively", name, a.Kind)
		}
		if !codexLocalImageMediaTypes[a.MediaType] {
			return nil, fmt.Errorf("codex: attachment %s: media type %q is not a codex localImage format", name, a.MediaType)
		}
		if a.Path == "" {
			return nil, fmt.Errorf("codex: attachment %s: no path; codex reads attachments from disk", name)
		}
		out = append(out, codexapp.LocalImage{
			Path:        a.Path,
			DisplayPath: a.DisplayPath,
			MediaType:   a.MediaType,
			Size:        a.Size,
		})
	}
	return out, nil
}

// InterruptTurn implements runtime.Runtime.
func (r *Runtime) InterruptTurn(ctx context.Context, sessionID, turnID string) error {
	return r.adapter.InterruptTurn(ctx, sessionID, turnID)
}

// SteerTurn implements runtime.Runtime.
func (r *Runtime) SteerTurn(ctx context.Context, sessionID, turnID, text string) error {
	return r.adapter.SteerTurn(ctx, sessionID, turnID, text)
}

// ResolveApproval implements runtime.Runtime. The response must carry the ID,
// SessionID, and TurnID from the KindApprovalNeeded event's payload; the adapter
// refuses a correlation that does not match exactly.
//
// ApprovalDecision.AllowFor is not honored: the app-server's decision vocabulary
// for a mediated request is accept-or-cancel for that one request, so a
// "session" or "project" grant would have to be enforced shipmates-side by a
// policy layer this runtime does not have. Allow is therefore always allow-once.
func (r *Runtime) ResolveApproval(ctx context.Context, resp runtime.ApprovalResponse, dec runtime.ApprovalDecision) (bool, error) {
	codexDecision := codexapp.Deny
	if dec.Allow {
		codexDecision = codexapp.AllowOnce
	}
	return r.adapter.ResolveApproval(ctx, codexapp.ApprovalResponse{
		RequestID: resp.ID,
		ThreadID:  resp.SessionID,
		TurnID:    resp.TurnID,
	}, codexDecision)
}

// Close implements runtime.Runtime.
func (r *Runtime) Close(ctx context.Context) error {
	r.stopped.Do(func() { close(r.stopFan) })
	return r.adapter.Close(ctx)
}

func (r *Runtime) rememberSession(id string, spec runtime.SessionSpec) *session {
	r.sessMu.Lock()
	s := &session{id: id, persona: spec.Persona, projectDir: spec.ProjectDir}
	r.sessions[id] = s
	r.sessMu.Unlock()
	return s
}

// fanout normalizes codex events into the shipmates-side event stream.
// Backend-only fields (raw codes, activity categories) are folded into
// KindBackend so consumers don't have to know codex internals.
func (r *Runtime) fanout() {
	defer close(r.stream)
	src := r.adapter.Events()
	for {
		select {
		case <-r.stopFan:
			return
		case ev, ok := <-src:
			if !ok {
				return
			}
			// The send is guarded by stopFan too: a consumer that stops reading
			// must not be able to pin this goroutine — and the adapter's own
			// readLoop behind it — alive past Close.
			select {
			case r.stream <- normalize(ev):
			case <-r.stopFan:
				return
			}
		}
	}
}

func normalize(ev codexapp.Event) runtime.Event {
	out := runtime.Event{
		Timestamp: time.Now(),
		SessionID: ev.ThreadID,
		TurnID:    ev.TurnID,
		Payload:   ev,
	}
	switch ev.Kind {
	case codexapp.AgentMessage:
		out.Kind = runtime.KindText
	case codexapp.Activity:
		out.Kind = runtime.KindToolCall
	case codexapp.ApprovalRequested:
		out.Kind = runtime.KindApprovalNeeded
		// The payload is the exact value ResolveApproval needs back. Without it a
		// consumer would have to import codexapp and reach into the raw event to
		// answer, which is precisely the coupling this interface exists to avoid.
		out.Payload = runtime.ApprovalResponse{
			ID:        ev.BackendRequestID,
			SessionID: ev.ThreadID,
			TurnID:    ev.TurnID,
			Kind:      approvalKind(ev.Category),
			Reason:    approvalReason(ev),
		}
	case codexapp.RequestRefused:
		out.Kind = runtime.KindError
	case codexapp.TurnCompleted:
		out.Kind = runtime.KindTurnDone
	case codexapp.TurnFailed, codexapp.AdapterFault:
		out.Kind = runtime.KindError
	default:
		out.Kind = runtime.KindBackend
	}
	return out
}

// approvalKind maps a codex activity category onto the
// runtime.ApprovalResponse.Kind vocabulary ("tool_use", "file_write", ...).
func approvalKind(category string) string {
	switch category {
	case "command":
		return "tool_use"
	case "file_change":
		return "file_write"
	default:
		return "tool_use"
	}
}

func approvalReason(ev codexapp.Event) string {
	if ev.CommandExact != "" {
		return ev.CommandExact
	}
	return ev.Detail
}

func threadOptsFromSpec(spec runtime.SessionSpec) codexapp.ThreadOptions {
	dir := spec.WorkingDir
	if dir == "" {
		dir = spec.ProjectDir
	}
	return codexapp.ThreadOptions{
		WorkingDirectory: dir,
		// DeveloperInstructions / Model / ReadOnly / Toolless come from the
		// persona/policy layer, not raw SessionSpec.
		// spec.Environment never reaches here: Start/ResumeSession reject a
		// non-empty value because ThreadOptions cannot carry it.
	}
}

// session is the runtime.Session implementation for codex threads.
type session struct {
	id, persona, projectDir string
}

func (s *session) ID() string         { return s.id }
func (s *session) Persona() string    { return s.persona }
func (s *session) ProjectDir() string { return s.projectDir }

// turn is the runtime.Turn implementation.
type turn struct {
	id, sessionID string
	startedAt     time.Time
}

func (t *turn) ID() string           { return t.id }
func (t *turn) SessionID() string    { return t.sessionID }
func (t *turn) StartedAt() time.Time { return t.startedAt }

// Compile-time assertion that Runtime satisfies runtime.Runtime.
var _ runtime.Runtime = (*Runtime)(nil)
