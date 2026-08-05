package openai

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// runtimeName is the name this runtime answers to in config and logs.
const runtimeName = "openai"

// eventBuffer is the depth of the shared event channel. Deep enough that a
// consumer doing per-delta rendering work does not stall generation, shallow
// enough that a consumer which has stopped reading applies backpressure
// instead of buffering an entire response.
const eventBuffer = 512

// Runtime is the OpenAI-compatible chat-completions runtime.
//
// One Runtime owns one endpoint + model pair, an HTTP client, a set of live
// sessions, and one event channel shared by all of them (as the interface
// specifies: consumers demultiplex on Event.SessionID/TurnID).
//
// Concurrency: safe for concurrent use. One turn at a time per session — the
// chat-completions API is stateless, so two turns racing on the same
// transcript would interleave into nonsense; a second SendTurn on a busy
// session is refused rather than queued, so the caller decides what to do
// about it.
type Runtime struct {
	cfg    Config
	client *client
	events chan runtime.Event

	// baseCtx outlives any single call. Turn contexts descend from it, not
	// from the ctx passed to SendTurn: that one is scoped to *queuing* the
	// turn and is typically cancelled the moment the caller's request
	// handler returns, which would kill the stream we just started.
	baseCtx context.Context
	cancel  context.CancelFunc

	mu           sync.Mutex
	sessions     map[string]*session
	closed       bool
	eventsClosed bool
	wg           sync.WaitGroup
	now          func() time.Time
}

var _ runtime.Runtime = (*Runtime)(nil)

// New builds a Runtime from an already-validated Config.
func New(cfg Config) (*Runtime, error) {
	c, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{
		cfg:      c.cfg,
		client:   c,
		events:   make(chan runtime.Event, eventBuffer),
		baseCtx:  ctx,
		cancel:   cancel,
		sessions: map[string]*session{},
		now:      time.Now,
	}, nil
}

// NewFromSettings is the factory-facing constructor: it has the shape
// runtime.Factory.New needs, so wiring is one line in the factory.
//
// ctx is used only for cancellation checks during construction; the returned
// Runtime keeps its own lifetime and is shut down with Close.
func NewFromSettings(ctx context.Context, settings map[string]any) (runtime.Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, err := ParseConfig(settings)
	if err != nil {
		return nil, err
	}
	return New(cfg)
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return runtimeName }

// Capabilities implements runtime.Runtime.
//
// Every field is a statement about what this runtime *actually does*, and the
// false ones are the important part — a caller that trusts an optimistic Caps
// presents the operator a supervision story that does not exist.
//
//   - Streaming true: SSE deltas become KindText events as they arrive.
//   - Interrupt true: InterruptTurn cancels the request context, which tears
//     down the HTTP connection. The server stops generating; this is not a
//     cosmetic "stop pretending to listen".
//   - Steer false: chat-completions has no channel for injecting an
//     instruction into an in-flight completion. SteerTurn returns
//     ErrUnsupported rather than faking it by queueing a follow-up turn.
//   - Attachments false: vision content-parts exist in the API but no
//     arbitrary OpenAI-compatible endpoint can be assumed to accept them, and
//     silently dropping an image is worse than refusing it.
//   - Refusal false: there is no dependable protocol refusal signal across
//     compatible servers. OpenAI's delta.refusal and finish_reason
//     "content_filter" are surfaced on TurnDone when present, but they are not
//     reliable enough to advertise as a capability.
//   - Containment false: this runtime spawns no processes at all, so there is
//     nothing for the containment watcher to hold. Reporting true would claim
//     operator limits apply to something that does not exist.
//   - Environment false: same reason. With no child process there is nothing
//     for SessionSpec.Environment to apply to, so a non-empty Environment is
//     rejected with ErrUnsupported instead of quietly ignored. (Note the one
//     env var that matters here — the API key — is read from the *shipmates*
//     process environment by name, which is a config decision, not a
//     per-session one.)
//   - Approvals false: a model emits no tool calls for this runtime to gate,
//     so ResolveApproval returns ErrUnsupported and callers must not present
//     this runtime as mediated. See toolloop_seam.go.
//
// Persona installation and memory are not Caps bits: this runtime does have a
// native persona artifact (see persona.go) and it does load persona memory —
// unconditionally, in StartSession, without a hook to install.
func (r *Runtime) Capabilities() runtime.Caps {
	return runtime.Caps{
		Streaming:   true,
		Interrupt:   true,
		Steer:       false,
		Attachments: false,
		Refusal:     false,
		Containment: false,
		Environment: false,
		Approvals:   false,
	}
}

// Model reports the configured model. Useful for status output; not part of
// the interface.
func (r *Runtime) Model() string { return r.cfg.Model }

// Endpoint reports the chat-completions URL in use. Contains no credential.
func (r *Runtime) Endpoint() string { return r.cfg.Endpoint() }

// Probe checks that the endpoint is reachable and credentials (if any) are
// accepted, by GETting base_url/models. Not part of runtime.Runtime — it
// exists so a `shipmates doctor` style check, or an integration test, can tell
// "endpoint is wrong" from "model is wrong" before a persona ever speaks.
// Returns the model IDs the endpoint advertises, which may legitimately be
// empty (a server with no models loaded still answers).
func (r *Runtime) Probe(ctx context.Context) ([]string, error) {
	return r.client.listModels(ctx)
}

// session is one persona conversation. The endpoint is stateless, so the
// transcript lives here and is sent in full on every turn.
type session struct {
	id         string
	persona    string
	projectDir string
	workingDir string
	system     string

	mu        sync.Mutex
	messages  []message
	bytes     int
	turn      *turnState
	closed    bool
	lastTrim  bool
	startedAt time.Time
}

func (s *session) ID() string         { return s.id }
func (s *session) Persona() string    { return s.persona }
func (s *session) ProjectDir() string { return s.projectDir }

type turnState struct {
	id          string
	startedAt   time.Time
	cancel      context.CancelFunc
	interrupted bool
}

// turnHandle implements runtime.Turn.
type turnHandle struct {
	id        string
	sessionID string
	startedAt time.Time
}

func (t turnHandle) ID() string           { return t.id }
func (t turnHandle) SessionID() string    { return t.sessionID }
func (t turnHandle) StartedAt() time.Time { return t.startedAt }

// ErrTurnInFlight is returned by SendTurn when the session already has a turn
// running. The transcript is ours to keep coherent; two concurrent
// completions against one transcript cannot be.
var ErrTurnInFlight = errors.New("openai runtime: this session already has a turn in flight")

// ErrTranscriptNotPersisted is returned by ResumeSession for a session ID this
// process does not hold in memory. See ResumeSession for the whole honest
// story.
var ErrTranscriptNotPersisted = errors.New("openai runtime: session transcripts are in-memory only and cannot be resumed across processes")

// ErrRuntimeClosed is returned once Close has been called.
var ErrRuntimeClosed = errors.New("openai runtime: runtime is closed")

// StartSession implements runtime.Runtime. It resolves the persona's system
// prompt (installed persona file plus a bounded memory digest, see
// systemPrompt) and creates an empty transcript. No network call is made — the
// first request happens on the first turn, so a session is cheap and a
// misconfigured endpoint surfaces at the turn that needed it.
func (r *Runtime) StartSession(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	// Unsupported first, before any other validation: the Caps contract says a
	// false capability must refuse cheaply and without side effects. There is
	// no child process here to apply an environment to.
	if len(spec.Environment) > 0 {
		return nil, runtime.Unsupported(runtimeName, "SessionSpec.Environment")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(spec.Persona) == "" {
		return nil, fmt.Errorf("openai runtime: SessionSpec.Persona is required")
	}

	sys, err := systemPrompt(spec.ProjectDir, spec.Persona, r.cfg.MaxSystemPromptBytes)
	if err != nil {
		return nil, err
	}

	id, err := newID("sess")
	if err != nil {
		return nil, err
	}
	workingDir := spec.WorkingDir
	if workingDir == "" {
		workingDir = spec.ProjectDir
	}
	s := &session{
		id:         id,
		persona:    spec.Persona,
		projectDir: spec.ProjectDir,
		workingDir: workingDir,
		system:     sys,
		startedAt:  r.now(),
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrRuntimeClosed
	}
	r.sessions[id] = s
	r.mu.Unlock()
	return s, nil
}

// ResumeSession implements runtime.Runtime — honestly, which here means
// mostly "no".
//
// The chat-completions API is stateless: there is no server-side conversation
// to reattach to, and the only transcript that exists is the one this process
// holds in memory. So:
//
//   - A session ID this Runtime still holds (same process, not yet closed) is
//     returned with its transcript intact, and the spec is checked against the
//     original so a caller cannot silently rebind a session to another
//     persona.
//   - Anything else returns ErrTranscriptNotPersisted. It does not start a
//     fresh session wearing the old ID; a caller who wants that can call
//     StartSession, and a human deserves to know their history is gone rather
//     than watch the crew member forget the conversation without saying so.
//
// If cross-process resume is wanted later, the transcript would need to be
// persisted under .shipmates/sessions/ — a durability and a
// what-do-we-write-to-disk decision (transcripts contain everything the
// operator typed), not a protocol one.
func (r *Runtime) ResumeSession(ctx context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	if len(spec.Environment) > 0 {
		return nil, runtime.Unsupported(runtimeName, "SessionSpec.Environment")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, ErrRuntimeClosed
	}
	s, ok := r.sessions[id]
	if !ok {
		return nil, fmt.Errorf("%w (session %q)", ErrTranscriptNotPersisted, id)
	}
	if spec.Persona != "" && spec.Persona != s.persona {
		return nil, fmt.Errorf("openai runtime: session %q belongs to persona %q, not %q", id, s.persona, spec.Persona)
	}
	return s, nil
}

// CloseSession implements runtime.Runtime. Cancels an in-flight turn, drops
// the transcript, and emits KindSessionClosed. Idempotent for an unknown ID —
// a caller cleaning up twice is not an error worth surfacing.
func (r *Runtime) CloseSession(ctx context.Context, id string) error {
	r.mu.Lock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	r.mu.Unlock()
	if !ok {
		return nil
	}
	s.mu.Lock()
	s.closed = true
	if s.turn != nil {
		s.turn.cancel()
	}
	s.messages = nil
	s.bytes = 0
	s.mu.Unlock()

	r.emitTerminal(runtime.Event{
		Timestamp: r.now(),
		Kind:      runtime.KindSessionClosed,
		SessionID: id,
		Payload:   SessionClosed{Reason: "closed by caller"},
	})
	return nil
}

// SendTurn implements runtime.Runtime. It appends the user message, starts one
// streaming request in a goroutine, and returns immediately with the turn
// handle; output arrives on Events().
//
// ctx bounds only the enqueue. The request itself runs under the runtime's own
// context plus Config.Timeout, so a caller whose HTTP handler returns does not
// truncate the answer.
func (r *Runtime) SendTurn(ctx context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	// Caps.Attachments is false, so refuse before anything else: an image the
	// model never saw would make for a confidently wrong answer.
	if len(in.Attachments) > 0 {
		return nil, runtime.Unsupported(runtimeName, "TurnInput.Attachments")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, fmt.Errorf("openai runtime: turn input text is empty")
	}
	if r.cfg.MaxPromptBytes > 0 && len(text) > r.cfg.MaxPromptBytes {
		return nil, fmt.Errorf("openai runtime: turn input is %d bytes, over the %d byte limit (max_prompt_bytes)", len(text), r.cfg.MaxPromptBytes)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, ErrRuntimeClosed
	}
	s, ok := r.sessions[sessionID]
	r.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("openai runtime: unknown session %q", sessionID)
	}

	turnID, err := newID("turn")
	if err != nil {
		return nil, err
	}
	started := r.now()

	var turnCtx context.Context
	var cancel context.CancelFunc
	if r.cfg.Timeout > 0 {
		turnCtx, cancel = context.WithTimeout(r.baseCtx, r.cfg.Timeout)
	} else {
		turnCtx, cancel = context.WithCancel(r.baseCtx)
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		cancel()
		return nil, fmt.Errorf("openai runtime: session %q is closed", sessionID)
	}
	if s.turn != nil {
		s.mu.Unlock()
		cancel()
		return nil, ErrTurnInFlight
	}
	ts := &turnState{id: turnID, startedAt: started, cancel: cancel}
	s.turn = ts
	s.appendLocked(message{Role: roleUser, Content: text}, r.cfg)
	req := chatRequest{
		Model:       r.cfg.Model,
		Messages:    s.requestMessagesLocked(),
		Stream:      true,
		Temperature: r.cfg.Temperature,
		MaxTokens:   r.cfg.MaxTokens,
	}
	trimmed := s.lastTrim
	s.mu.Unlock()

	r.wg.Add(1)
	go r.runTurn(turnCtx, s, ts, req, trimmed)

	return turnHandle{id: turnID, sessionID: sessionID, startedAt: started}, nil
}

// runTurn drives one request to completion and guarantees the event contract:
// zero or more KindText, at most one KindError, exactly one KindTurnDone, in
// that order.
func (r *Runtime) runTurn(ctx context.Context, s *session, ts *turnState, req chatRequest, transcriptTrimmed bool) {
	defer r.wg.Done()
	defer ts.cancel()

	emitText := func(delta string) {
		r.emitDelta(ctx, runtime.Event{
			Timestamp: r.now(),
			Kind:      runtime.KindText,
			SessionID: s.id,
			TurnID:    ts.id,
			Payload:   TextDelta{Text: delta},
		})
	}
	emitReasoning := func(delta string) {
		r.emitDelta(ctx, runtime.Event{
			Timestamp: r.now(),
			Kind:      runtime.KindBackend,
			SessionID: s.id,
			TurnID:    ts.id,
			Payload:   ReasoningDelta{Text: delta},
		})
	}

	res, err := r.client.streamChat(ctx, req, emitText, emitReasoning)

	// Whatever text arrived becomes part of the transcript, including a
	// partial answer from an interrupted turn: the operator saw those tokens,
	// so the model's next turn should see them too.
	s.mu.Lock()
	interrupted := ts.interrupted
	if !s.closed {
		if res.Text != "" {
			s.appendLocked(message{Role: roleAssistant, Content: res.Text}, r.cfg)
			transcriptTrimmed = transcriptTrimmed || s.lastTrim
		}
		if s.turn == ts {
			s.turn = nil
		}
	}
	s.mu.Unlock()

	done := TurnDone{
		Text:              res.Text,
		FinishReason:      res.FinishReason,
		Refusal:           res.Refusal,
		Interrupted:       interrupted,
		Truncated:         res.Truncated,
		TranscriptTrimmed: transcriptTrimmed,
		MalformedChunks:   res.MalformedChunks,
		Usage:             res.Usage,
		Model:             res.Model,
		Duration:          r.now().Sub(ts.startedAt),
	}
	if done.Model == "" {
		done.Model = r.cfg.Model
	}

	if err != nil {
		switch {
		case interrupted && (errors.Is(err, context.Canceled) || isConnectionAborted(err)):
			// A cancelled request is the interrupt working, not a failure.
			// Some transports report the torn-down connection instead of the
			// context error, hence the second test.
			done.Truncated = true
		case errors.Is(err, context.DeadlineExceeded):
			done.Failed = true
			r.emitError(s.id, ts.id, ErrorEvent{
				Message: fmt.Sprintf("openai runtime: turn exceeded the %s timeout", r.cfg.Timeout),
			})
		case errors.Is(err, context.Canceled):
			// The runtime (not this turn) was shut down underneath us.
			done.Failed = true
			r.emitError(s.id, ts.id, ErrorEvent{Message: "openai runtime: turn cancelled"})
		default:
			done.Failed = true
			r.emitError(s.id, ts.id, errorEventFor(err))
		}
	}

	r.emitTerminal(runtime.Event{
		Timestamp: r.now(),
		Kind:      runtime.KindTurnDone,
		SessionID: s.id,
		TurnID:    ts.id,
		Payload:   done,
	})
}

// errorEventFor maps a client error onto the ErrorEvent payload, keeping the
// endpoint's own taxonomy when it supplied one. Everything here has already
// been scrubbed of the API key by the client.
func errorEventFor(err error) ErrorEvent {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return ErrorEvent{
			Message:    apiErr.Error(),
			HTTPStatus: apiErr.Status,
			Code:       apiErr.Code,
			Type:       apiErr.Type,
			Retryable:  apiErr.Retryable(),
		}
	}
	return ErrorEvent{Message: err.Error()}
}

// isConnectionAborted recognises the transport-level shrapnel of a cancelled
// request: platform-specific reset/abort strings that carry no context error.
func isConnectionAborted(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"context canceled",
		"connection reset by peer",
		"forcibly closed by the remote host", // Windows WSAECONNRESET wording
		"use of closed network connection",
		"client connection force closed",
		"http2: stream closed",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

// InterruptTurn implements runtime.Runtime, for real: it cancels the turn's
// context, which aborts the in-flight HTTP request. Partial output already
// emitted stays emitted, TurnDone follows with Interrupted set.
func (r *Runtime) InterruptTurn(ctx context.Context, sessionID, turnID string) error {
	r.mu.Lock()
	s, ok := r.sessions[sessionID]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("openai runtime: unknown session %q", sessionID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil || (turnID != "" && s.turn.id != turnID) {
		return fmt.Errorf("openai runtime: no turn %q in flight on session %q", turnID, sessionID)
	}
	s.turn.interrupted = true
	s.turn.cancel()
	return nil
}

// SteerTurn implements runtime.Runtime by refusing: chat-completions offers no
// way to inject an instruction into a completion that is already generating.
// Queueing the text as a follow-up turn would look like steering while being
// something else, so callers get ErrUnsupported and can decide for themselves.
func (r *Runtime) SteerTurn(ctx context.Context, sessionID, turnID, text string) error {
	return runtime.Unsupported(runtimeName, "SteerTurn")
}

// Events implements runtime.Runtime. All sessions share one channel;
// demultiplex on SessionID/TurnID.
//
// Consumers must drain it. Delta sends are dropped if the turn is cancelled
// while a send is pending, but terminal events (KindTurnDone,
// KindSessionClosed) block until the buffer has room or the runtime is closed,
// because losing a turn boundary would hang a caller waiting for one.
func (r *Runtime) Events() <-chan runtime.Event { return r.events }

// ResolveApproval implements runtime.Runtime by refusing. There are no
// runtime-issued approval requests to resolve: this runtime executes nothing.
// Reporting anything else would let shipmates present an unmediated model as a
// mediated agent.
func (r *Runtime) ResolveApproval(ctx context.Context, resp runtime.ApprovalResponse, d runtime.ApprovalDecision) (bool, error) {
	return false, runtime.Unsupported(runtimeName, "ResolveApproval")
}

// Close implements runtime.Runtime. Cancels every in-flight turn, waits for
// the turn goroutines to finish, then closes the event channel. Idempotent.
func (r *Runtime) Close(ctx context.Context) error {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil
	}
	r.closed = true
	sessions := make([]*session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	r.sessions = map[string]*session{}
	r.mu.Unlock()

	for _, s := range sessions {
		s.mu.Lock()
		s.closed = true
		if s.turn != nil {
			s.turn.cancel()
		}
		s.mu.Unlock()
	}
	r.cancel()

	waited := make(chan struct{})
	go func() {
		r.wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-ctx.Done():
		// Leave the channel open: a turn goroutine may still be running and
		// closing under it would panic. The runtime is unusable either way.
		return ctx.Err()
	}

	r.mu.Lock()
	if !r.eventsClosed {
		r.eventsClosed = true
		close(r.events)
	}
	r.mu.Unlock()
	// Release idle keep-alive connections to the endpoint.
	if tr, ok := r.client.hc.Transport.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
	return nil
}

// emitDelta sends a streaming event, giving up if the turn is cancelled while
// the buffer is full. Dropping a delta from a turn nobody is waiting on any
// more is better than wedging the turn goroutine.
func (r *Runtime) emitDelta(ctx context.Context, ev runtime.Event) {
	select {
	case r.events <- ev:
	case <-ctx.Done():
	case <-r.baseCtx.Done():
	}
}

// emitTerminal sends an event that a consumer may be blocking on. It waits
// for buffer space and only gives up when the whole runtime is shutting down.
func (r *Runtime) emitTerminal(ev runtime.Event) {
	select {
	case r.events <- ev:
	case <-r.baseCtx.Done():
	}
}

func (r *Runtime) emitError(sessionID, turnID string, payload ErrorEvent) {
	r.emitTerminal(runtime.Event{
		Timestamp: r.now(),
		Kind:      runtime.KindError,
		SessionID: sessionID,
		TurnID:    turnID,
		Payload:   payload,
	})
}

// --- transcript -------------------------------------------------------------

// appendLocked adds a message and enforces both transcript bounds by dropping
// the oldest messages. The system prompt is not in this slice (it is rebuilt
// per request from session.system), so trimming can never drop the persona.
//
// Callers must hold s.mu. Sets s.lastTrim so the turn can report the loss.
func (s *session) appendLocked(m message, cfg Config) {
	s.messages = append(s.messages, m)
	s.bytes += len(m.Content) + len(m.Role)
	s.lastTrim = false

	maxBytes := cfg.MaxTranscriptBytes
	maxMsgs := cfg.MaxTranscriptMessages
	for len(s.messages) > 1 && ((maxMsgs > 0 && len(s.messages) > maxMsgs) || (maxBytes > 0 && s.bytes > maxBytes)) {
		drop := s.messages[0]
		s.bytes -= len(drop.Content) + len(drop.Role)
		s.messages = s.messages[1:]
		s.lastTrim = true
	}
	// A transcript that now starts with an assistant message reads as the
	// model talking to itself; drop those too so the history stays a
	// conversation.
	for len(s.messages) > 1 && s.messages[0].Role == roleAssistant {
		drop := s.messages[0]
		s.bytes -= len(drop.Content) + len(drop.Role)
		s.messages = s.messages[1:]
		s.lastTrim = true
	}
}

// requestMessagesLocked builds the outgoing message list: the system prompt
// followed by a copy of the transcript. A copy, because the request runs in
// another goroutine while this session keeps mutating.
//
// Callers must hold s.mu.
func (s *session) requestMessagesLocked() []message {
	out := make([]message, 0, len(s.messages)+1)
	if s.system != "" {
		out = append(out, message{Role: roleSystem, Content: s.system})
	}
	out = append(out, s.messages...)
	return out
}

// transcriptLen reports the number of non-system messages held. Test and
// status helper.
func (s *session) transcriptLen() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.messages)
}

// newID returns a prefixed random identifier. Random rather than sequential so
// two shipmates processes talking to one endpoint cannot collide on IDs that
// end up in the same room transcript.
func newID(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("openai runtime: generating id: %w", err)
	}
	return runtimeName + "-" + prefix + "-" + hex.EncodeToString(b[:]), nil
}
