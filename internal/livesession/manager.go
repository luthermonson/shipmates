// Package livesession owns backend-neutral, per-persona live process state.
// It deliberately has no CLI, HTTP, feed, steering, or interruption surface.
package livesession

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/turninput"
)

type State string

const (
	Starting     State = "starting"
	Idle         State = "idle"
	Working      State = "working"
	Steering     State = "steering"
	Interrupting State = "interrupting"
	Stopped      State = "stopped"
	Failed       State = "failed"
)

type Code string

const (
	Busy                Code = "busy"
	NotFound            Code = "not_found"
	NotSteerable        Code = "not_steerable"
	InvalidTarget       Code = "invalid_target"
	StaleTarget         Code = "stale_target"
	StoppedCode         Code = "stopped"
	FailedCode          Code = "failed"
	Internal            Code = "internal"
	AlreadyInterrupting Code = "already_interrupting"
	AlreadyTerminal     Code = "already_terminal"
	AlreadyOpen         Code = "already_open"
	StaleController     Code = "stale_controller"
	StartingCode        Code = "starting"
	InvalidInput        Code = "invalid_input"
	InputTooLarge       Code = "input_too_large"
	ApprovalPending     Code = "approval_pending"
	StaleApproval       Code = "stale_approval"
	AlreadyResolved     Code = "already_resolved"
	ResolutionFailed    Code = "resolution_failed"
	ApprovalConcurrent  Code = "approval_concurrent"
	DuplicateApproval   Code = "duplicate_approval"
	RequestIDReuse      Code = "request_id_reuse"
)

const feedCapacity = 256
const maxMessageBytes = 16 << 10

type Event struct {
	SchemaVersion int            `json:"schema_version"`
	Sequence      uint64         `json:"sequence"`
	Time          string         `json:"time"`
	Persona       string         `json:"persona"`
	SessionID     string         `json:"session_id"`
	ThreadID      string         `json:"thread_id,omitempty"`
	TurnID        string         `json:"turn_id,omitempty"`
	Kind          string         `json:"kind"`
	Data          map[string]any `json:"data,omitempty"`
}
type Feed struct {
	Events          []Event `json:"events"`
	OldestAvailable uint64  `json:"oldest_available"`
	NextSequence    uint64  `json:"next_sequence"`
	HistoryDropped  bool    `json:"history_dropped"`
}
type ControlResult struct {
	Code     Code     `json:"code"`
	Snapshot Snapshot `json:"target"`
}

type ControllerAction string

const (
	ControllerMessage   ControllerAction = "message"
	ControllerInterrupt ControllerAction = "interrupt"
)

// ControlController validates interactive authority and the complete target
// captured by the client before dispatching an action. Validation never
// redirects an old controller or turn to current state.
func (m *Manager) ControlController(ctx context.Context, action ControllerAction, persona, controllerID, sessionID, threadID, turnID, text string) (ControlResult, error) {
	return m.ControlControllerInput(ctx, action, persona, controllerID, sessionID, threadID, turnID, text, nil)
}
func (m *Manager) ControlControllerInput(ctx context.Context, action ControllerAction, persona, controllerID, sessionID, threadID, turnID, text string, images []turninput.ImageDescriptorV1) (ControlResult, error) {
	s, err := m.Session(persona)
	if err != nil {
		return ControlResult{}, err
	}
	s.mu.Lock()
	now := m.now()
	if controllerID == "" || controllerID != s.controllerID || !now.Before(s.controllerExpires) {
		if controllerID == s.controllerID {
			s.controllerID = ""
		}
		s.mu.Unlock()
		return ControlResult{}, failure(StaleController)
	}
	snap := s.snapshotLocked()
	if sessionID != snap.SessionID || threadID != snap.ThreadID || turnID != snap.TurnID {
		s.mu.Unlock()
		return ControlResult{}, failure(StaleTarget)
	}
	state := s.state
	s.mu.Unlock()
	if action == ControllerMessage {
		if !validControllerMessage(text) {
			return ControlResult{}, failure(InvalidInput)
		}
		if len(text) > maxMessageBytes {
			return ControlResult{}, failure(InputTooLarge)
		}
	}

	switch action {
	case ControllerMessage:
		switch state {
		case Idle:
			return m.StartNextTurnInput(ctx, persona, sessionID, threadID, text, images)
		case Working:
			if len(images) != 0 {
				return ControlResult{}, failure(InvalidInput)
			}
			return m.Tell(ctx, persona, sessionID, threadID, turnID, text)
		case Starting:
			return ControlResult{}, failure(StartingCode)
		case Steering, Interrupting:
			return ControlResult{}, failure(Busy)
		case Stopped:
			return ControlResult{}, failure(StoppedCode)
		case Failed:
			return ControlResult{}, failure(FailedCode)
		}
	case ControllerInterrupt:
		if state == Idle {
			return ControlResult{Code: AlreadyTerminal, Snapshot: snap}, nil
		}
		return m.Interrupt(ctx, persona, sessionID, threadID, turnID)
	}
	return ControlResult{}, failure(Internal)
}

func validControllerMessage(text string) bool {
	return text != "" && utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool {
		return r < 0x20 && r != '\t' && r != '\n'
	}) < 0
}

type Attach struct {
	Snapshot
	ControllerID              string        `json:"controller_id"`
	ControllerLeaseGeneration uint64        `json:"controller_lease_generation"`
	OldestAvailable           uint64        `json:"oldest_available"`
	NextSequence              uint64        `json:"next_sequence"`
	HistoryDropped            bool          `json:"history_dropped"`
	Events                    []Event       `json:"events"`
	LeaseTimeoutMS            int64         `json:"lease_timeout_ms"`
	PendingApproval           *ApprovalCard `json:"pending_approval,omitempty"`
}

type Error struct{ Code Code }

func (e *Error) Error() string { return string(e.Code) }
func failure(c Code) error     { return &Error{Code: c} }

// StartAdapter starts the codex app-server transport. It is the codex-only
// half of backend construction; non-codex runtimes come from the Manager's
// runtime.Selector instead (see resolveRuntime).
type StartAdapter func(context.Context, codexapp.StartOptions) (Backend, codexapp.Capabilities, error)

// codexStart wraps the production codexapp factory into a StartAdapter,
// keeping the typed-nil out of the Backend interface on failure.
func codexStart(ctx context.Context, opts codexapp.StartOptions) (Backend, codexapp.Capabilities, error) {
	a, caps, err := (codexapp.Factory{}).Start(ctx, opts)
	if err != nil {
		return nil, caps, err
	}
	return a, caps, nil
}

type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	start    StartAdapter
	// selector picks the runtime for a new live session. nil keeps every
	// session on the codex app-server transport, which is what every
	// direct-manager caller (sail, recovery, tests) wants.
	selector runtime.Selector
	// cliRuntime is an explicit `--runtime` override forwarded to the
	// selector. Empty means "whatever config resolves to".
	cliRuntime      string
	adapterOptions  codexapp.StartOptions
	newSessionID    func() (string, error)
	newControllerID func() (string, error)
	newApprovalID   func() (string, error)
	now             func() time.Time
	after           func(time.Duration) <-chan time.Time
	leaseDuration   time.Duration
	loadPolicy      func(string, string, string) (*policy.Snapshot, []policy.Diagnostic)
	projectRoot     string
}

type StartOptions struct {
	Persona               string
	Prompt                string
	Fresh                 bool
	Images                []turninput.ImageDescriptorV1
	Config                *project.PersonaConfig
	ExposeActivityDetails bool
	ReadOnly              bool
	Toolless              bool
	DeveloperInstructions string
}

// StartIdleOptions establishes an owned thread without sending model input.
type StartIdleOptions struct {
	Persona               string
	Fresh                 bool
	Config                *project.PersonaConfig
	ExposeActivityDetails bool
	ReadOnly              bool
	Toolless              bool
	DeveloperInstructions string
}

type Snapshot struct {
	Persona     string        `json:"persona"`
	SessionID   string        `json:"session_id"`
	Backend     string        `json:"backend"`
	ThreadID    string        `json:"thread_id"`
	TurnID      string        `json:"turn_id,omitempty"`
	State       State         `json:"state"`
	FailureCode codexapp.Code `json:"failure_code,omitempty"`
}

type Session struct {
	mu                                   sync.Mutex
	persona, sessionID, threadID, turnID string
	// backend names the transport this session runs on ("codex-app-server",
	// "claude"). It is fixed at StartIdle and reported in every snapshot.
	backend                              string
	state                                State
	failureCode                          codexapp.Code
	adapter                              Backend
	release                              func()
	done                                 chan struct{}
	doneOnce                             sync.Once
	shutting                             bool
	events                               []Event
	nextSequence                         uint64
	notify                               chan struct{}
	lastTerminal                         string
	turnStarting                         bool
	pendingTurnEvents                    []codexapp.Event
	controllerID                         string
	controllerLeaseGeneration            uint64
	controllerExpires                    time.Time
	policySnapshot                       *policy.Snapshot
	effort                               string
	exposeActivityDetails                bool
	approval                             *pendingApproval
	approvalHistory                      map[string]approvalRecord
	beginApproval                        func(NormalizedApprovalRequest, ApprovalEvaluation, ApprovalAuthority, approvalAdapter)
	approvalNow                          func() time.Time
}

func New(start StartAdapter, adapterOptions codexapp.StartOptions) *Manager {
	return NewAt("", start, adapterOptions)
}

// NewAt binds all project filesystem access to root. An empty root preserves
// the direct-manager convention of capturing the caller's current directory.
// Sessions run on the codex app-server transport; see NewWithRuntime for a
// manager that honors the operator's runtime selection.
func NewAt(root string, start StartAdapter, adapterOptions codexapp.StartOptions) *Manager {
	if start == nil {
		start = codexStart
	}
	return &Manager{sessions: make(map[string]*Session), start: start, adapterOptions: adapterOptions, newSessionID: randomID, newControllerID: randomID, newApprovalID: randomID, now: time.Now, after: time.After, leaseDuration: 15 * time.Second, loadPolicy: policy.Load, projectRoot: root}
}

// NewWithRuntime is NewAt plus a runtime.Selector, so new live sessions run
// on whatever runtime `--runtime` / SHIPMATES_RUNTIME / .shipmates/config.yaml
// resolves to. A nil selector, or a selection that resolves to codex, keeps
// the codex app-server transport unchanged.
func NewWithRuntime(root string, sel runtime.Selector, cliRuntime string, start StartAdapter, adapterOptions codexapp.StartOptions) *Manager {
	m := NewAt(root, start, adapterOptions)
	m.selector, m.cliRuntime = sel, cliRuntime
	return m
}

// resolveRuntime asks the Selector which runtime a new live session should
// use. It deliberately does NOT spawn anything: codex is reported as a nil
// Runtime so the app-server transport is still started later, under the
// dispatch lock, exactly where it always was.
func (m *Manager) resolveRuntime(ctx context.Context, cwd string) (runtime.Runtime, string, error) {
	if m.selector == nil {
		return nil, project.CodexAppServerBackend, nil
	}
	rt, _, err := m.selector.Select(ctx, cwd, m.cliRuntime)
	if err != nil {
		var notConfigured *runtime.ErrNotConfigured
		if errors.As(err, &notConfigured) && notConfigured.Runtime == "codex" {
			// The Selector's by-design answer for codex: it cannot carry
			// codexapp.StartOptions. Fall through to the native transport.
			return nil, project.CodexAppServerBackend, nil
		}
		return nil, "", err
	}
	if rt.Name() == "codex" {
		_ = rt.Close(ctx)
		return nil, project.CodexAppServerBackend, nil
	}
	return rt, rt.Name(), nil
}

func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

// AttachController atomically grants the sole interactive controller lease and
// captures state plus retained history. Feed observers acquire no authority.
func (m *Manager) AttachController(persona, sessionID string, after uint64) (Attach, error) {
	s, err := m.Session(persona)
	if err != nil {
		return Attach{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != "" && sessionID != s.sessionID {
		return Attach{}, failure(StaleTarget)
	}
	if s.state == Stopped {
		return Attach{}, failure(StoppedCode)
	}
	if s.state == Failed {
		return Attach{}, failure(FailedCode)
	}
	if s.state == Starting || s.threadID == "" {
		return Attach{}, failure(StartingCode)
	}
	now := m.now()
	if s.controllerID != "" && !now.Before(s.controllerExpires) {
		s.cancelApprovalLocked("lease_expired", false)
		s.controllerID = ""
	}
	if s.controllerID != "" {
		return Attach{}, failure(AlreadyOpen)
	}
	id, err := m.newControllerID()
	if err != nil {
		return Attach{}, failure(Internal)
	}
	s.controllerID = id
	if s.controllerLeaseGeneration == ^uint64(0) {
		s.controllerID = ""
		return Attach{}, failure(Internal)
	}
	s.controllerLeaseGeneration++
	s.controllerExpires = now.Add(m.leaseDuration)
	f := s.feedLocked(after)
	return Attach{Snapshot: s.snapshotLocked(), ControllerID: id, ControllerLeaseGeneration: s.controllerLeaseGeneration, OldestAvailable: f.OldestAvailable, NextSequence: f.NextSequence, HistoryDropped: f.HistoryDropped, Events: f.Events, LeaseTimeoutMS: m.leaseDuration.Milliseconds(), PendingApproval: s.approvalCardLocked(now)}, nil
}

// ReclaimExpiredController performs only the authoritative expiry transition
// needed by a bounded attach retry. It never revokes a live lease, allocates a
// replacement generation, or changes session/turn state.
func (m *Manager) ReclaimExpiredController(persona, sessionID string) (bool, error) {
	s, err := m.Session(persona)
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != "" && sessionID != s.sessionID {
		return false, failure(StaleTarget)
	}
	if s.controllerID == "" || m.now().Before(s.controllerExpires) {
		return false, nil
	}
	s.cancelApprovalLocked("lease_expired", false)
	s.controllerID = ""
	s.controllerExpires = time.Time{}
	return true, nil
}

func (m *Manager) HeartbeatController(persona, sessionID, controllerID string) error {
	s, err := m.Session(persona)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := m.now()
	if sessionID != s.sessionID || controllerID == "" || controllerID != s.controllerID || !now.Before(s.controllerExpires) {
		if controllerID == s.controllerID {
			s.cancelApprovalLocked("lease_expired", false)
			s.controllerID = ""
		}
		return failure(StaleController)
	}
	s.controllerExpires = now.Add(m.leaseDuration)
	return nil
}

// SyncController atomically renews the lease and captures current state plus
// retained events from the controller's private reconnect cursor.
func (m *Manager) SyncController(persona, sessionID, controllerID string, after uint64) (Attach, error) {
	s, err := m.Session(persona)
	if err != nil {
		return Attach{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := m.now()
	if sessionID != s.sessionID || controllerID == "" || controllerID != s.controllerID || !now.Before(s.controllerExpires) {
		if controllerID == s.controllerID {
			s.cancelApprovalLocked("lease_expired", false)
			s.controllerID = ""
		}
		return Attach{}, failure(StaleController)
	}
	s.controllerExpires = now.Add(m.leaseDuration)
	f := s.feedLocked(after)
	return Attach{Snapshot: s.snapshotLocked(), ControllerID: controllerID, ControllerLeaseGeneration: s.controllerLeaseGeneration, OldestAvailable: f.OldestAvailable, NextSequence: f.NextSequence, HistoryDropped: f.HistoryDropped, Events: f.Events, LeaseTimeoutMS: m.leaseDuration.Milliseconds(), PendingApproval: s.approvalCardLocked(now)}, nil
}

// ResolveControllerApproval requires both the live outer controller credential
// and the complete authority tuple captured with its private card.
func (m *Manager) ResolveControllerApproval(ctx context.Context, persona, controllerID, sessionID string, auth ApprovalAuthority, decision ApprovalDecision) (ApprovalResult, error) {
	if auth.Persona != persona || auth.SessionID != sessionID || auth.ControllerID != controllerID {
		return ApprovalResult{}, failure(StaleApproval)
	}
	return m.ResolveApproval(ctx, auth, decision)
}

// ReleaseController revokes only interactive authority. It does not mutate the
// session lifecycle, child, feed, continuity, or dispatch-lock ownership.
func (m *Manager) ReleaseController(persona, sessionID, controllerID string) error {
	s, err := m.Session(persona)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID != s.sessionID {
		return failure(StaleTarget)
	}
	if controllerID == "" || controllerID != s.controllerID {
		return failure(StaleController)
	}
	s.cancelApprovalLocked("controller_released", false)
	s.controllerID = ""
	s.controllerExpires = time.Time{}
	return nil
}

// StartLive preserves the M3 start-with-first-turn operation.
func (m *Manager) StartLive(ctx context.Context, opts StartOptions) (*Session, error) {
	if opts.Prompt == "" {
		return nil, failure(Internal)
	}
	s, err := m.StartIdle(ctx, StartIdleOptions{Persona: opts.Persona, Fresh: opts.Fresh, Config: opts.Config, ExposeActivityDetails: opts.ExposeActivityDetails, ReadOnly: opts.ReadOnly, Toolless: opts.Toolless, DeveloperInstructions: opts.DeveloperInstructions})
	if err != nil {
		return nil, err
	}
	snap := s.Snapshot()
	if _, err := m.StartNextTurnInput(ctx, snap.Persona, snap.SessionID, snap.ThreadID, opts.Prompt, opts.Images); err != nil {
		// StartLive has always been all-or-nothing. Keep that public M3 behavior;
		// callers wanting an idle owner use StartIdle directly.
		s.finish(Failed, s.adapter, codexapp.ErrorCode(err))
		return nil, err
	}
	return s, nil
}

// StartIdle owns the dispatch lock before reading continuity or spawning. It
// acknowledges only after a confirmed thread is durably recorded and never
// calls turn/start or sends model input.
func (m *Manager) StartIdle(ctx context.Context, opts StartIdleOptions) (*Session, error) {
	root := m.projectRoot
	if root == "" {
		var err error
		root, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	if err := project.ValidatePersonaName(opts.Persona); err != nil {
		return nil, err
	}
	if _, err := project.InstalledPersonaPathAt(root, opts.Persona); err != nil {
		return nil, err
	}
	var cfg project.PersonaConfig
	if opts.Config != nil {
		cfg = *opts.Config
	} else {
		var err error
		cfg, err = project.ResolvePersonaConfigAt(root, opts.Persona)
		if err != nil {
			return nil, err
		}
	}
	var err error
	instructions := opts.DeveloperInstructions
	if instructions == "" {
		instructions, err = project.CodexPersonaInstructionsAt(root, opts.Persona)
		if err != nil {
			return nil, err
		}
	}
	cwd := root
	sandbox := "workspace-write"
	if opts.ReadOnly {
		sandbox = "read-only"
	}
	// Which runtime this session runs on is part of its identity: the
	// continuity marker, the config fingerprint and every published event
	// name it, so a runtime switch can never resume the other runtime's
	// thread. Resolution here constructs nothing for codex, so the
	// app-server spawn still happens below, under the dispatch lock.
	rt, backendName, err := m.resolveRuntime(ctx, cwd)
	if err != nil {
		return nil, err
	}
	closeRuntime := func() {
		if rt != nil {
			_ = rt.Close(context.Background())
		}
	}
	fingerprint := project.SHA([]byte(backendName + "\x00" + cfg.Fingerprint() + "\x00" + instructions + "\x00" + cwd + "\x00" + sandbox + "\x00never"))

	m.mu.Lock()
	if current := m.sessions[opts.Persona]; current != nil {
		current.mu.Lock()
		active := current.state != Stopped && current.state != Failed
		current.mu.Unlock()
		if active {
			m.mu.Unlock()
			closeRuntime()
			return nil, failure(Busy)
		}
	}
	sid, err := m.newSessionID()
	if err != nil {
		m.mu.Unlock()
		closeRuntime()
		return nil, failure(Internal)
	}
	s := &Session{persona: opts.Persona, sessionID: sid, backend: backendName, state: Starting, done: make(chan struct{}), nextSequence: 1, notify: make(chan struct{}, 1), effort: cfg.Effort, exposeActivityDetails: opts.ExposeActivityDetails}
	s.approvalNow = m.now
	s.beginApproval = func(r NormalizedApprovalRequest, e ApprovalEvaluation, auth ApprovalAuthority, a approvalAdapter) {
		if _, err := m.BeginApproval(r, e, auth, a); err != nil {
			code := ErrorCode(err)
			if code != ApprovalConcurrent && code != DuplicateApproval && code != RequestIDReuse {
				ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
				defer cancel()
				_, _ = a.ResolveApproval(ctx, AdapterApprovalRequest{r.BackendRequestID, r.ThreadID, r.TurnID, e.PolicySnapshotID}, DenyOnce)
			}
			s.mu.Lock()
			if s.threadID == r.ThreadID && s.turnID == r.TurnID && s.policySnapshot != nil && s.policySnapshot.ID == e.PolicySnapshotID {
				reason := "mediation_unavailable"
				if code == ApprovalConcurrent {
					reason = "approval_concurrent"
				}
				s.publishLocked("request.refused", r.ThreadID, r.TurnID, map[string]any{"request_class": string(policy.ProcessExec), "runtime_outcome": "refused", "policy_snapshot_id": e.PolicySnapshotID, "policy_effect": string(e.Effect), "reason_code": reason, "matched_rules": e.MatchedRules})
			}
			s.mu.Unlock()
		}
	}
	s.publishLocked("session.starting", "", "", map[string]any{"backend": backendName})
	m.sessions[opts.Persona] = s
	m.mu.Unlock()

	release, err := project.AcquireDispatchLockAt(root, opts.Persona)
	if err != nil {
		closeRuntime()
		s.finish(Failed, nil, codexapp.Internal)
		return nil, failure(Busy)
	}
	s.mu.Lock()
	s.release = release
	s.mu.Unlock()
	marker, have, err := project.ReadLiveContinuityBackendAt(root, opts.Persona, backendName)
	if err != nil {
		closeRuntime()
		s.startupFailure(ctx, nil, codexapp.Internal)
		return nil, err
	}
	resume := have && !opts.Fresh && marker.ConfigFingerprint == fingerprint

	var a Backend
	if rt != nil {
		a = newRuntimeBackend(rt, opts.Persona, cwd)
	} else {
		aopts := m.adapterOptions
		aopts.WorkingDirectory = cwd
		a, _, err = m.start(ctx, aopts)
		if err != nil {
			s.startupFailure(ctx, nil, codexapp.ErrorCode(err))
			return nil, err
		}
	}
	s.mu.Lock()
	s.adapter = a
	s.mu.Unlock()
	topts := codexapp.ThreadOptions{WorkingDirectory: cwd, DeveloperInstructions: instructions, Model: cfg.Model, ReadOnly: opts.ReadOnly, Toolless: opts.Toolless}
	var thread codexapp.Thread
	if resume {
		thread, err = a.ResumeThread(ctx, marker.ThreadID, topts)
	} else {
		thread, err = a.StartThread(ctx, topts)
	}
	if err != nil {
		s.startupFailure(ctx, a, codexapp.ErrorCode(err))
		return nil, err
	}
	confirmed := project.LiveContinuity{SchemaVersion: project.LiveContinuitySchema, Backend: backendName, ThreadID: thread.ID, ConfigFingerprint: fingerprint}
	if err := project.WriteLiveContinuityBackendAt(root, opts.Persona, backendName, confirmed); err != nil {
		s.startupFailure(ctx, a, codexapp.Internal)
		return nil, err
	}
	s.mu.Lock()
	if ctx.Err() != nil {
		s.mu.Unlock()
		s.startupFailure(ctx, a, codexapp.RequestTimeout)
		return nil, ctx.Err()
	}
	s.threadID, s.state = thread.ID, Idle
	if resume {
		s.publishLocked("thread.resumed", thread.ID, "", nil)
	} else {
		s.publishLocked("thread.started", thread.ID, "", map[string]any{"fresh": true})
	}
	s.publishLocked("session.ready", thread.ID, "", map[string]any{"resumed": resume})
	s.mu.Unlock()
	go s.monitor()
	return s, nil
}

// StartNextTurn starts one turn on an exact owned idle tuple. It never queues or
// redirects a request and publishes working state only after backend acceptance.
func (m *Manager) StartNextTurn(ctx context.Context, persona, sessionID, threadID, text string) (ControlResult, error) {
	return m.StartNextTurnInput(ctx, persona, sessionID, threadID, text, nil)
}

// StartNextTurnInput starts one exact idle turn with already validated local
// images. Images are neither queued nor retained by the session owner.
func (m *Manager) StartNextTurnInput(ctx context.Context, persona, sessionID, threadID, text string, images []turninput.ImageDescriptorV1) (ControlResult, error) {
	if text == "" {
		return ControlResult{}, failure(Internal)
	}
	s, err := m.Session(persona)
	if err != nil {
		return ControlResult{}, err
	}
	s.mu.Lock()
	if s.approval != nil {
		s.mu.Unlock()
		return ControlResult{}, failure(ApprovalPending)
	}
	snap := s.snapshotLocked()
	if sessionID != snap.SessionID || threadID != snap.ThreadID {
		s.mu.Unlock()
		return ControlResult{}, failure(StaleTarget)
	}
	switch s.state {
	case Stopped:
		s.mu.Unlock()
		return ControlResult{}, failure(StoppedCode)
	case Failed:
		s.mu.Unlock()
		return ControlResult{}, failure(FailedCode)
	case Idle:
	default:
		s.mu.Unlock()
		return ControlResult{}, failure(Busy)
	}
	if s.turnStarting {
		s.mu.Unlock()
		return ControlResult{}, failure(Busy)
	}
	s.turnStarting = true
	a := s.adapter
	s.mu.Unlock()

	root := m.projectRoot
	if root == "" {
		var rootErr error
		root, rootErr = os.Getwd()
		if rootErr != nil {
			s.mu.Lock()
			s.turnStarting = false
			s.publishLocked("turn.start_refused", threadID, "", map[string]any{"reason_code": "invalid_policy", "diagnostics": []map[string]any{{"code": "internal"}}})
			s.mu.Unlock()
			return ControlResult{}, failure(Internal)
		}
	}
	rootID, rootIDErr := project.ScopeID(root)
	if rootIDErr != nil {
		s.mu.Lock()
		s.turnStarting = false
		s.publishLocked("turn.start_refused", threadID, "", map[string]any{"reason_code": "invalid_policy", "diagnostics": []map[string]any{{"code": "internal"}}})
		s.mu.Unlock()
		return ControlResult{}, failure(Internal)
	}
	policySnapshot, diagnostics := m.loadPolicy(root, persona, rootID)
	if policySnapshot == nil {
		safe := make([]map[string]any, 0, len(diagnostics))
		for _, d := range diagnostics {
			safe = append(safe, map[string]any{"code": d.Code, "layer": d.Layer, "path": d.Path, "line": d.Line, "column": d.Column})
		}
		s.mu.Lock()
		s.turnStarting = false
		s.publishLocked("turn.start_refused", threadID, "", map[string]any{"reason_code": "invalid_policy", "diagnostics": safe})
		s.mu.Unlock()
		return ControlResult{}, failure(InvalidInput)
	}

	turn, err := a.StartTurn(ctx, threadID, codexapp.TurnInput{Text: text, Policy: policySnapshot, Images: images, Effort: s.effort})
	s.mu.Lock()
	s.turnStarting = false
	if err != nil {
		code := codexapp.ErrorCode(err)
		// Notifications for a turn the backend claims it rejected make process
		// health/acceptance ambiguous and therefore fail closed.
		uncertain := len(s.pendingTurnEvents) != 0 || code == codexapp.RequestTimeout || code == codexapp.MalformedFrame || code == codexapp.ProtocolViolation || code == codexapp.UnexpectedEOF || code == codexapp.ChildExit
		s.pendingTurnEvents = nil
		s.mu.Unlock()
		if uncertain {
			s.finish(Failed, a, code)
		}
		return ControlResult{}, err
	}
	// Terminal cleanup or an adapter fault may have won while the RPC was in
	// flight. Never resurrect or retarget such a session.
	if s.state != Idle || s.sessionID != sessionID || s.threadID != threadID {
		s.mu.Unlock()
		return ControlResult{}, failure(StaleTarget)
	}
	s.turnID, s.state = turn.ID, Working
	s.policySnapshot = policySnapshot
	data := map[string]any(nil)
	if len(images) != 0 {
		data = map[string]any{"image_count": len(images)}
	}
	s.publishLocked("turn.started", threadID, turn.ID, data)
	pending := s.pendingTurnEvents
	s.pendingTurnEvents = nil
	var approvalEvents []codexapp.Event
	for _, event := range pending {
		if event.Kind == codexapp.ApprovalRequested {
			approvalEvents = append(approvalEvents, event)
			continue
		}
		if !s.applyAdapterEventLocked(event) {
			s.mu.Unlock()
			s.finish(Failed, a, codexapp.ProtocolViolation)
			return ControlResult{}, failure(FailedCode)
		}
	}
	result := ControlResult{Snapshot: s.snapshotLocked()}
	s.mu.Unlock()
	for _, event := range approvalEvents {
		s.handleApprovalRequest(event)
	}
	return result, nil
}

func (s *Session) startupFailure(ctx context.Context, a Backend, code codexapp.Code) {
	state := Failed
	if ctx.Err() != nil {
		state = Stopped
	}
	s.finish(state, a, code)
}

func (s *Session) monitor() {
	for event := range s.adapter.Events() {
		s.mu.Lock()
		if s.state == Stopped || s.state == Failed || s.shutting {
			s.mu.Unlock()
			continue
		}
		if event.Kind == codexapp.AdapterFault {
			s.mu.Unlock()
			s.finish(Failed, s.adapter, event.Code)
			return
		}
		if s.turnStarting {
			if len(s.pendingTurnEvents) == feedCapacity {
				s.mu.Unlock()
				s.finish(Failed, s.adapter, codexapp.ProtocolViolation)
				return
			}
			s.pendingTurnEvents = append(s.pendingTurnEvents, event)
			s.mu.Unlock()
			continue
		}
		if event.Kind == codexapp.ApprovalRequested {
			s.mu.Unlock()
			s.handleApprovalRequest(event)
			continue
		}
		if !s.applyAdapterEventLocked(event) {
			s.mu.Unlock()
			s.finish(Failed, s.adapter, codexapp.ProtocolViolation)
			return
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	terminal := s.state == Stopped || s.state == Failed || s.shutting
	s.mu.Unlock()
	if !terminal {
		s.finish(Failed, s.adapter, codexapp.UnexpectedEOF)
	}
}

type codexApprovalAdapter struct{ adapter Backend }

func (a codexApprovalAdapter) ResolveApproval(ctx context.Context, r AdapterApprovalRequest, d ApprovalDecision) (AdapterApprovalResult, error) {
	decision := codexapp.Deny
	if d == AllowOnce {
		decision = codexapp.AllowOnce
	}
	ok, err := a.adapter.ResolveApproval(ctx, codexapp.ApprovalResponse{RequestID: r.RequestID, ThreadID: r.ThreadID, TurnID: r.TurnID, PolicySnapshotID: r.PolicySnapshotID}, decision)
	return AdapterApprovalResult{Accepted: ok}, err
}

func (s *Session) handleApprovalRequest(event codexapp.Event) {
	a := codexApprovalAdapter{s.adapter}
	s.mu.Lock()
	if event.ThreadID != s.threadID || event.TurnID != s.turnID || s.policySnapshot == nil || event.PolicySnapshotID != s.policySnapshot.ID {
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
		defer cancel()
		_, _ = a.ResolveApproval(ctx, AdapterApprovalRequest{event.BackendRequestID, event.ThreadID, event.TurnID, event.PolicySnapshotID}, DenyOnce)
		return
	}
	if event.PolicyEffect == policy.Allow {
		s.publishLocked("request.allowed", event.ThreadID, event.TurnID, map[string]any{"request_class": string(policy.ProcessExec), "runtime_outcome": "allowed", "policy_snapshot_id": event.PolicySnapshotID, "policy_effect": string(event.PolicyEffect), "matched_rules": event.MatchedRules})
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
		defer cancel()
		_, _ = a.ResolveApproval(ctx, AdapterApprovalRequest{event.BackendRequestID, event.ThreadID, event.TurnID, event.PolicySnapshotID}, AllowOnce)
		return
	}
	if event.PolicyEffect == policy.Deny {
		s.publishLocked("request.denied", event.ThreadID, event.TurnID, map[string]any{"request_class": string(policy.ProcessExec), "runtime_outcome": "denied", "policy_snapshot_id": event.PolicySnapshotID, "policy_effect": string(event.PolicyEffect), "matched_rules": event.MatchedRules})
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
		defer cancel()
		_, _ = a.ResolveApproval(ctx, AdapterApprovalRequest{event.BackendRequestID, event.ThreadID, event.TurnID, event.PolicySnapshotID}, DenyOnce)
		return
	}
	if s.controllerID == "" || !s.approvalNow().Before(s.controllerExpires) {
		s.publishLocked("request.refused", event.ThreadID, event.TurnID, map[string]any{"request_class": string(policy.ProcessExec), "runtime_outcome": "refused", "policy_snapshot_id": event.PolicySnapshotID, "policy_effect": string(event.PolicyEffect), "reason_code": "mediation_unavailable", "matched_rules": event.MatchedRules})
		s.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
		defer cancel()
		_, _ = a.ResolveApproval(ctx, AdapterApprovalRequest{event.BackendRequestID, event.ThreadID, event.TurnID, event.PolicySnapshotID}, DenyOnce)
		return
	}
	auth := ApprovalAuthority{Persona: s.persona, SessionID: s.sessionID, Backend: s.snapshotLocked().Backend,
		ThreadID: s.threadID, TurnID: s.turnID, ControllerID: s.controllerID,
		ControllerLeaseGeneration: s.controllerLeaseGeneration, BackendRequestID: event.BackendRequestID}
	r := NormalizedApprovalRequest{Kind: policy.ProcessExec, CommandExact: event.CommandExact,
		ProjectRootID: s.policySnapshot.ProjectRootID, SessionID: s.sessionID, ThreadID: s.threadID,
		TurnID: s.turnID, BackendRequestID: event.BackendRequestID}
	e := ApprovalEvaluation{Effect: event.PolicyEffect, PolicySnapshotID: event.PolicySnapshotID, MatchedRules: event.MatchedRules}
	s.mu.Unlock()
	// Manager lookup is safe here, but the session does not retain its manager.
	// The owner callback installed at construction supplies the state-machine entry.
	if s.beginApproval != nil {
		s.beginApproval(r, e, auth, a)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), ApprovalResponseDeadline)
	defer cancel()
	_, _ = a.ResolveApproval(ctx, AdapterApprovalRequest{event.BackendRequestID, event.ThreadID, event.TurnID, event.PolicySnapshotID}, DenyOnce)
}

func (s *Session) applyAdapterEventLocked(event codexapp.Event) bool {
	if event.ThreadID != s.threadID || event.TurnID != s.turnID {
		return false
	}
	switch event.Kind {
	case codexapp.RequestRefused:
		// Adapter requests are advisory protocol traffic, never authority. Only
		// the exact currently active tuple bound to this session's immutable
		// snapshot may be represented in its audit feed; delayed/mismatched
		// refusals are safely ignored. A legitimate request may have been buffered
		// while the correlated turn/start acknowledgement was being installed.
		if s.policySnapshot == nil || event.PolicySnapshotID != s.policySnapshot.ID {
			return true
		}
		data := map[string]any{"request_class": event.RequestClass, "runtime_outcome": "refused"}
		if event.PolicySnapshotID == "" {
			data["reason_code"] = "unsupported_request"
		} else {
			data["policy_snapshot_id"], data["policy_effect"], data["reason_code"] = event.PolicySnapshotID, event.PolicyEffect, event.ReasonCode
			data["matched_rules"], data["omitted_matches"] = event.MatchedRules, event.OmittedMatches
		}
		s.publishLocked("request.refused", event.ThreadID, event.TurnID, data)
	case codexapp.AgentMessage:
		text, truncated := boundedText(event.Text, maxMessageBytes)
		if text != "" {
			s.publishLocked("agent.message", s.threadID, s.turnID, map[string]any{"text": text, "partial": event.Partial, "truncated": truncated})
		}
	case codexapp.Activity:
		data := map[string]any{"category": event.Category}
		if s.exposeActivityDetails && event.Detail != "" {
			detail, _ := boundedText(event.Detail, maxMessageBytes)
			data["detail"] = detail
		}
		s.publishLocked("activity", s.threadID, s.turnID, data)
	case codexapp.TurnCompleted:
		if s.state == Interrupting {
			s.lastTerminal = "interrupted"
			s.publishLocked("turn.interrupted", s.threadID, s.turnID, map[string]any{"reason": "operator"})
		} else {
			s.lastTerminal = "completed"
			s.publishLocked("turn.completed", s.threadID, s.turnID, map[string]any{"outcome": "completed"})
		}
	case codexapp.TurnFailed:
		s.lastTerminal = "failed"
		s.publishLocked("turn.failed", s.threadID, s.turnID, map[string]any{"code": "turn_failed", "message": "the turn failed", "retryable": true})
	}
	if event.Kind == codexapp.TurnCompleted || event.Kind == codexapp.TurnFailed {
		s.cancelApprovalLocked("turn_terminal", false)
		s.turnID = ""
		s.state = Idle
	}
	return true
}

func boundedText(v string, limit int) (string, bool) {
	v = strings.ToValidUTF8(v, "�")
	v = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, v)
	if len(v) <= limit {
		return v, false
	}
	for limit > 0 && (v[limit]&0xc0) == 0x80 {
		limit--
	}
	return v[:limit], true
}

func (s *Session) publishLocked(kind, threadID, turnID string, data map[string]any) {
	e := Event{SchemaVersion: 1, Sequence: s.nextSequence, Time: time.Now().UTC().Format(time.RFC3339Nano), Persona: s.persona, SessionID: s.sessionID, ThreadID: threadID, TurnID: turnID, Kind: kind, Data: data}
	s.nextSequence++
	if len(s.events) == feedCapacity {
		copy(s.events, s.events[1:])
		s.events[len(s.events)-1] = e
	} else {
		s.events = append(s.events, e)
	}
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

func (s *Session) Feed(after uint64) Feed {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.feedLocked(after)
}

func (s *Session) feedLocked(after uint64) Feed {
	oldest := s.nextSequence
	if len(s.events) > 0 {
		oldest = s.events[0].Sequence
	}
	f := Feed{OldestAvailable: oldest, NextSequence: s.nextSequence, HistoryDropped: after+1 < oldest}
	for _, e := range s.events {
		if e.Sequence > after {
			f.Events = append(f.Events, e)
		}
	}
	return f
}
func (s *Session) Notify() <-chan struct{} { return s.notify }

func (m *Manager) Session(persona string) (*Session, error) {
	if err := project.ValidatePersonaName(persona); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[persona]
	if s == nil {
		return nil, failure(NotFound)
	}
	return s, nil
}
func (m *Manager) ShutdownAll(ctx context.Context) error {
	m.mu.Lock()
	list := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		list = append(list, s)
	}
	m.mu.Unlock()
	var first error
	for _, s := range list {
		if err := s.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (m *Manager) Tell(ctx context.Context, persona, sid, tid, turn, text string) (ControlResult, error) {
	return m.TellInput(ctx, persona, sid, tid, turn, text, nil)
}

// TellInput is Tell with attachments folded into the same mid-turn message.
// Backends that can carry an attachment mid-turn take it natively; the rest
// never receive one, because the caller renders images as path references
// for those transports instead (see attach.Reference).
func (m *Manager) TellInput(ctx context.Context, persona, sid, tid, turn, text string, images []turninput.ImageDescriptorV1) (ControlResult, error) {
	if strings.TrimSpace(text) == "" {
		return ControlResult{}, failure(Internal)
	}
	s, err := m.Session(persona)
	if err != nil {
		return ControlResult{}, err
	}
	s.mu.Lock()
	if s.approval != nil {
		s.mu.Unlock()
		return ControlResult{}, failure(ApprovalPending)
	}
	snap := s.snapshotLocked()
	if sid != snap.SessionID || tid != snap.ThreadID || turn != snap.TurnID {
		s.mu.Unlock()
		return ControlResult{}, failure(StaleTarget)
	}
	if s.state == Steering || s.state == Interrupting {
		s.mu.Unlock()
		return ControlResult{}, failure(Busy)
	}
	if s.state != Working {
		s.mu.Unlock()
		return ControlResult{}, failure(NotSteerable)
	}
	s.state = Steering
	a := s.adapter
	s.mu.Unlock()
	if len(images) == 0 {
		err = a.SteerTurn(ctx, tid, turn, text)
	} else if steerer, ok := a.(attachmentSteerer); ok {
		err = steerer.SteerTurnInput(ctx, tid, turn, codexapp.TurnInput{Text: text, Images: images})
	} else {
		err = failure(InvalidInput)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == Steering {
		s.state = Working
	}
	if err != nil {
		s.publishLocked("steering.refused", tid, turn, map[string]any{"code": "refused", "message": "steering was refused"})
		return ControlResult{}, err
	}
	data := map[string]any(nil)
	if len(images) != 0 {
		data = map[string]any{"image_count": len(images)}
	}
	s.publishLocked("steering.accepted", tid, turn, data)
	return ControlResult{Snapshot: s.snapshotLocked()}, nil
}

// SteerExactTurn is the real M3 boundary used after RemoteSteerCoordinator has
// durably claimed an operation and moved the exact session to Steering. It
// deliberately does not choose a current turn or acquire controller authority.
func (m *Manager) SteerExactTurn(ctx context.Context, sid, tid, turn, text string) RemoteSteerDecision {
	m.mu.Lock()
	candidates := make([]*Session, 0, len(m.sessions))
	for _, candidate := range m.sessions {
		candidates = append(candidates, candidate)
	}
	m.mu.Unlock()
	var s *Session
	for _, candidate := range candidates {
		candidate.mu.Lock()
		match := candidate.sessionID == sid && candidate.threadID == tid && candidate.turnID == turn
		candidate.mu.Unlock()
		if match {
			if s != nil { // Session IDs are authority identifiers and must be unique.
				return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "stale_target"}
			}
			s = candidate
		}
	}
	if s == nil || strings.TrimSpace(text) == "" {
		return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "stale_target"}
	}
	s.mu.Lock()
	if s.sessionID != sid || s.threadID != tid || s.turnID != turn {
		s.mu.Unlock()
		return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "stale_target"}
	}
	if s.approval != nil {
		s.mu.Unlock()
		return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "approval_pending"}
	}
	if s.state != Steering || s.shutting {
		reason := "busy"
		if s.shutting {
			reason = "shutdown"
		}
		s.mu.Unlock()
		return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: reason}
	}
	a := s.adapter
	s.mu.Unlock()
	if a == nil {
		return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "not_steerable"}
	}
	err := a.SteerTurn(ctx, tid, turn, text)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.publishLocked("steering.refused", tid, turn, map[string]any{"code": "refused", "message": "steering was refused"})
		if ctx.Err() != nil {
			return RemoteSteerDecision{Outcome: RemoteSteerIndeterminate, ReasonCode: "internal_uncertain"}
		}
		return RemoteSteerDecision{Outcome: RemoteSteerRefused, ReasonCode: "not_steerable"}
	}
	s.publishLocked("steering.accepted", tid, turn, nil)
	return RemoteSteerDecision{Outcome: RemoteSteerAccepted, ReasonCode: "accepted"}
}

func (m *Manager) Interrupt(ctx context.Context, persona, sid, tid, turn string) (ControlResult, error) {
	if turn == "" {
		return ControlResult{}, failure(InvalidTarget)
	}
	s, err := m.Session(persona)
	if err != nil {
		return ControlResult{}, err
	}
	s.mu.Lock()
	snap := s.snapshotLocked()
	if sid != snap.SessionID || tid != snap.ThreadID || turn != snap.TurnID {
		s.mu.Unlock()
		return ControlResult{}, failure(StaleTarget)
	}
	if s.state == Interrupting {
		s.mu.Unlock()
		return ControlResult{Code: AlreadyInterrupting, Snapshot: snap}, nil
	}
	if s.state == Idle {
		s.mu.Unlock()
		return ControlResult{Code: AlreadyTerminal, Snapshot: snap}, nil
	}
	if s.state != Working && s.state != Steering {
		s.mu.Unlock()
		return ControlResult{}, failure(NotSteerable)
	}
	s.state = Interrupting
	s.cancelApprovalLocked("interrupt", false)
	a := s.adapter
	active := s.turnID
	s.mu.Unlock()
	if err := a.InterruptTurn(ctx, tid, active); err != nil {
		s.mu.Lock()
		// Steering is a transient RPC state. If interrupt raced with a steer,
		// restoring Steering after that RPC returned would permanently wedge the
		// turn. Restore the stable active state only while this exact turn still
		// owns the interrupt transition; a terminal monitor event wins the race.
		if s.state == Interrupting && s.threadID == tid && s.turnID == active {
			s.state = Working
			s.publishLocked("interrupt.refused", tid, active, map[string]any{
				"code":      string(codexapp.ErrorCode(err)),
				"message":   "interruption was refused",
				"retryable": true,
			})
		}
		s.mu.Unlock()
		return ControlResult{}, err
	}
	return ControlResult{Snapshot: s.Snapshot()}, nil
}

// CancelPendingApproval is the synchronous M9 deny-class boundary. It selects
// denial under the session lock, then waits only for the existing bounded M6
// delivery. No remote approval identity or decision enters this method.
func (m *Manager) CancelPendingApproval(ctx context.Context, sid, tid, turn string) ApprovalCancelOutcome {
	m.mu.Lock()
	candidates := make([]*Session, 0, len(m.sessions))
	for _, candidate := range m.sessions {
		candidates = append(candidates, candidate)
	}
	m.mu.Unlock()
	var s *Session
	for _, candidate := range candidates {
		candidate.mu.Lock()
		match := candidate.sessionID == sid && candidate.threadID == tid && candidate.turnID == turn
		candidate.mu.Unlock()
		if match {
			s = candidate
			break
		}
	}
	if s == nil {
		return ApprovalAbsent
	}
	s.mu.Lock()
	p := s.approval
	if p == nil {
		s.mu.Unlock()
		return ApprovalAbsent
	}
	if p.committed {
		done := p.done
		s.mu.Unlock()
		select {
		case <-done:
			return ApprovalCancelled
		case <-ctx.Done():
			return ApprovalUnknown
		}
	}
	p.decision, p.actor, p.reason, p.committed = DenyOnce, "system", "remote_interrupt", true
	s.mu.Unlock()
	resolveCtx, cancel := context.WithTimeout(ctx, ApprovalResponseDeadline)
	defer cancel()
	result, err := p.adapter.ResolveApproval(resolveCtx, adapterRequest(p), DenyOnce)
	ok := err == nil && result.Accepted
	s.completeApproval(p, ok, false)
	if !ok {
		return ApprovalUnknown
	}
	return ApprovalCancelled
}

// InterruptExactTurn invokes the real M3 adapter boundary only for the exact
// turn already serialized into Interrupting by the M9 coordinator.
func (m *Manager) InterruptExactTurn(ctx context.Context, sid, tid, turn string) ExactTurnInterruptOutcome {
	m.mu.Lock()
	var s *Session
	for _, candidate := range m.sessions {
		candidate.mu.Lock()
		match := candidate.sessionID == sid && candidate.threadID == tid && candidate.turnID == turn && candidate.state == Interrupting
		candidate.mu.Unlock()
		if match {
			s = candidate
			break
		}
	}
	m.mu.Unlock()
	if s == nil {
		return ExactTurnAlreadyTerminal
	}
	s.mu.Lock()
	a := s.adapter
	s.mu.Unlock()
	if a == nil || a.InterruptTurn(ctx, tid, turn) != nil {
		return ExactTurnUnknown
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		s.mu.Lock()
		same := s.threadID == tid && s.turnID == turn
		state := s.state
		s.mu.Unlock()
		if !same || state == Idle {
			return ExactTurnInterrupted
		}
		if state == Failed || state == Stopped {
			return ExactTurnUnknown
		}
		select {
		case <-ctx.Done():
			return ExactTurnUnknown
		case <-deadline.C:
			return ExactTurnUnknown
		case <-tick.C:
		}
	}
}

func (m *Manager) ActiveRemoteInterruptTargets(now time.Time) []RemoteInterruptTarget {
	steer := m.ActiveRemoteSteerTargets(now)
	out := make([]RemoteInterruptTarget, 0, len(steer))
	for _, t := range steer {
		out = append(out, RemoteInterruptTarget{Reference: CurrentRemoteInterruptTargetReference(t.Persona, t.SessionID, t.ThreadID, t.TurnID), Persona: t.Persona, SessionID: t.SessionID, Backend: t.Backend, ThreadID: t.ThreadID, TurnID: t.TurnID, ExpiresAt: t.ExpiresAt})
	}
	return out
}

func CurrentRemoteInterruptTargetReference(persona, sessionID, threadID, turnID string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("shipmates/fleet-interrupt-target/v1\x00"))
	for _, v := range []string{persona, sessionID, threadID, turnID} {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	return "itr_" + hex.EncodeToString(h.Sum(nil)[:32])
}

func (s *Session) snapshotLocked() Snapshot {
	backend := s.backend
	if backend == "" {
		backend = project.CodexAppServerBackend
	}
	return Snapshot{Persona: s.persona, SessionID: s.sessionID, Backend: backend, ThreadID: s.threadID, TurnID: s.turnID, State: s.state, FailureCode: s.failureCode}
}

func (s *Session) finish(state State, a Backend, code codexapp.Code) {
	if a != nil {
		_ = a.Close(context.Background())
	}
	s.doneOnce.Do(func() {
		s.mu.Lock()
		s.cancelApprovalLocked("session_terminal", false)
		if state == Stopped {
			s.publishLocked("session.stopped", s.threadID, s.turnID, map[string]any{"reason": "requested"})
		}
		if state == Failed {
			s.publishLocked("session.failed", s.threadID, s.turnID, map[string]any{"code": string(code), "message": "the live session failed", "retryable": true})
		}
		s.state, s.failureCode, s.turnID, s.shutting = state, code, "", false
		s.controllerID = ""
		s.controllerExpires = time.Time{}
		release := s.release
		s.release = nil
		s.mu.Unlock()
		if release != nil {
			release()
		}
		close(s.done)
	})
}

func (s *Session) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	a := s.adapter
	terminal := s.state == Stopped || s.state == Failed
	threadID, turnID := s.threadID, s.turnID
	if !terminal {
		s.shutting = true
	}
	s.mu.Unlock()
	if terminal {
		return nil
	}
	var err error
	if a != nil {
		if turnID != "" {
			_ = a.InterruptTurn(ctx, threadID, turnID)
		}
		err = a.Close(ctx)
	}
	if err != nil {
		s.finish(Failed, nil, codexapp.CleanupFailed)
		return err
	}
	s.finish(Stopped, nil, "")
	return nil
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *Session) Done() <-chan struct{} { return s.done }

// ProcessGroupHandle returns the real child-process-group cleanup seam for
// bounded local supervisors. It exposes no process identity or protocol data.
func (s *Session) ProcessGroupHandle() codexapp.ProcessGroupHandle {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.adapter == nil {
		return nil
	}
	return s.adapter.ProcessGroupHandle()
}

// ResolveTarget is the internal identity gate future control operations must
// cross. It never redirects a stale session/thread/turn tuple to a newer owner.
func (m *Manager) ResolveTarget(persona, sessionID, threadID, turnID string) (Snapshot, error) {
	if err := project.ValidatePersonaName(persona); err != nil {
		return Snapshot{}, err
	}
	m.mu.Lock()
	s := m.sessions[persona]
	m.mu.Unlock()
	if s == nil {
		return Snapshot{}, failure(NotFound)
	}
	snap := s.Snapshot()
	if sessionID != snap.SessionID || threadID != snap.ThreadID || (turnID != "" && turnID != snap.TurnID) {
		return Snapshot{}, failure(StaleTarget)
	}
	switch snap.State {
	case Stopped:
		return Snapshot{}, failure(StoppedCode)
	case Failed:
		return Snapshot{}, failure(FailedCode)
	}
	return snap, nil
}

// ActiveRemoteSteerTargets is the server-owned, read-only projection used by
// the separate ship observer process. Private tuples never come from disk or
// the observer and are revalidated again by RemoteSteerCoordinator.
func (m *Manager) ActiveRemoteSteerTargets(now time.Time) []RemoteSteerTarget {
	m.mu.Lock()
	personas := make([]string, 0, len(m.sessions))
	for persona := range m.sessions {
		personas = append(personas, persona)
	}
	m.mu.Unlock()
	sort.Strings(personas)
	out := make([]RemoteSteerTarget, 0, len(personas))
	for _, persona := range personas {
		s, err := m.Session(persona)
		if err != nil {
			continue
		}
		snap := s.Snapshot()
		if snap.State != Working || snap.SessionID == "" || snap.ThreadID == "" || snap.TurnID == "" {
			continue
		}
		out = append(out, RemoteSteerTarget{Reference: CurrentRemoteTargetReference(persona, snap.SessionID, snap.ThreadID, snap.TurnID), Persona: persona, SessionID: snap.SessionID, Backend: snap.Backend, ThreadID: snap.ThreadID, TurnID: snap.TurnID, ExpiresAt: now.Add(60 * time.Second)})
	}
	return out
}

func CurrentRemoteTargetReference(persona, sessionID, threadID, turnID string) string {
	h := sha256.New()
	_, _ = h.Write([]byte("shipmates/fleet-steer-target/v1\x00"))
	for _, v := range []string{persona, sessionID, threadID, turnID} {
		_, _ = h.Write([]byte(v))
		_, _ = h.Write([]byte{0})
	}
	return "trg_" + hex.EncodeToString(h.Sum(nil)[:16])
}

func ErrorCode(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	var adapterErr *codexapp.Error
	if errors.As(err, &adapterErr) {
		return Code(adapterErr.Code)
	}
	return Internal
}
