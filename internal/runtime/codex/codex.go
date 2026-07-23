// Package codex wraps internal/codexapp.Adapter to satisfy the
// runtime.Runtime interface. The adapter's method names and shapes are close
// to the interface already (StartThread → StartSession, StartTurn →
// SendTurn, etc.); this file is thin glue plus event normalization.
package codex

import (
	"context"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// Runtime is the codex-backed runtime.Runtime. Construct via New.
type Runtime struct {
	adapter *codexapp.Adapter
	caps    codexapp.Capabilities
	stream  chan runtime.Event
	stopFan chan struct{}
	stopped sync.Once

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
		adapter:  adapter,
		caps:     caps,
		stream:   make(chan runtime.Event, 64),
		stopFan:  make(chan struct{}),
		sessions: map[string]*session{},
	}
	go r.fanout()
	return r, nil
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "codex" }

// Capabilities implements runtime.Runtime.
func (r *Runtime) Capabilities() runtime.Caps {
	return runtime.Caps{
		Streaming:   true,
		Interrupt:   r.caps.Interrupt,
		Steer:       r.caps.Steer,
		Attachments: r.caps.LocalImage,
		Refusal:     r.caps.RequestRefusal,
		// Containment reflects whether the transport was started with
		// RequireExecutionContainment. The adapter doesn't currently expose
		// that flag post-init, so we conservatively report false and rely
		// on the caller's config to know they asked for it.
		Containment: false,
	}
}

// Events implements runtime.Runtime.
func (r *Runtime) Events() <-chan runtime.Event { return r.stream }

// StartSession implements runtime.Runtime.
func (r *Runtime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	th, err := r.adapter.StartThread(ctx, threadOptsFromSpec(spec))
	if err != nil {
		return nil, err
	}
	return r.rememberSession(th.ID, spec), nil
}

// ResumeSession implements runtime.Runtime.
func (r *Runtime) ResumeSession(ctx context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
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

// SendTurn implements runtime.Runtime.
func (r *Runtime) SendTurn(ctx context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	// Attachments → codexapp.TurnInput.Images requires attachment-side
	// translation (image descriptor open/hashing). That plumbing lives in
	// internal/turninput and is not yet wired here; text-only turns work.
	t, err := r.adapter.StartTurn(ctx, sessionID, codexapp.TurnInput{Text: in.Text})
	if err != nil {
		return nil, err
	}
	return &turn{id: t.ID, sessionID: sessionID, startedAt: time.Now()}, nil
}

// InterruptTurn implements runtime.Runtime.
func (r *Runtime) InterruptTurn(ctx context.Context, sessionID, turnID string) error {
	return r.adapter.InterruptTurn(ctx, sessionID, turnID)
}

// SteerTurn implements runtime.Runtime.
func (r *Runtime) SteerTurn(ctx context.Context, sessionID, turnID, text string) error {
	return r.adapter.SteerTurn(ctx, sessionID, turnID, text)
}

// ResolveApproval implements runtime.Runtime.
func (r *Runtime) ResolveApproval(ctx context.Context, resp runtime.ApprovalResponse, dec runtime.ApprovalDecision) (bool, error) {
	codexDecision := codexapp.Deny
	if dec.Allow {
		codexDecision = codexapp.AllowOnce
	}
	return r.adapter.ResolveApproval(ctx, codexapp.ApprovalResponse{
		RequestID:        resp.ID,
		ThreadID:         resp.SessionID,
		TurnID:           resp.TurnID,
		PolicySnapshotID: "",
	}, codexDecision)
}

// InstallPersona writes .codex/agents/<name>.md for the requested persona.
// TODO(phase 4): pull persona shape from PersonaSpec once the canonical
// catalog format lands. For now the branch's existing
// installer/catalog code owns persona provisioning; this method is a
// forward-looking hook.
func (r *Runtime) InstallPersona(context.Context, string, runtime.PersonaSpec) error {
	return &runtime.ErrUnsupported{Runtime: "codex", Feature: "InstallPersona (Phase 4)"}
}

// UninstallPersona removes .codex/agents/<name>.md. See InstallPersona
// TODO — same deferral applies.
func (r *Runtime) UninstallPersona(context.Context, string, string) error {
	return &runtime.ErrUnsupported{Runtime: "codex", Feature: "UninstallPersona (Phase 4)"}
}

// InstallMemoryHook wires the codex session-start memory injection. The
// mechanism is codex-specific and not yet plumbed through the wrapper.
// TODO(phase 4): mirror what claude does with SessionStart hooks.
func (r *Runtime) InstallMemoryHook(context.Context, string) error {
	return &runtime.ErrUnsupported{Runtime: "codex", Feature: "InstallMemoryHook (Phase 4)"}
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
// Backend-only fields (policy snapshots, raw codes) are folded into
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
			r.stream <- normalize(ev)
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

func threadOptsFromSpec(spec runtime.SessionSpec) codexapp.ThreadOptions {
	return codexapp.ThreadOptions{
		WorkingDirectory: spec.WorkingDir,
		// DeveloperInstructions / Model / ReadOnly / Toolless come from the
		// persona/policy layer, not raw SessionSpec — plumbed in Phase 3+.
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
