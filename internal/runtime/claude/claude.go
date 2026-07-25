// Package claude implements runtime.Runtime by driving the `claude` CLI
// (Anthropic Claude Code) via non-interactive stdio streaming.
//
// Sessions map to one persistent `claude -p --input-format stream-json
// --output-format stream-json` process each, spawned lazily on the first
// SendTurn. Turns are user-message JSONL lines written to the child's
// stdin; the per-turn `result` frame on stdout marks the turn boundary.
// Because stdin stays open for the process lifetime, mid-turn steering
// (additional user messages) and protocol-level interrupt
// (`control_request` / `control_response`) are both supported.
//
// Protocol notes (verified against claude 2.1.153):
//
//   - A user turn is one stdin line:
//     {"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
//   - Each user turn eventually produces one stdout line
//     {"type":"result","subtype":"success"|"error_during_execution",…,"is_error":bool}
//   - A user message written while a turn is in flight is folded INTO that
//     turn (single result frame); written between turns it starts a new turn.
//   - Interrupt: {"type":"control_request","request_id":"…","request":{"subtype":"interrupt"}}
//     is answered by {"type":"control_response","response":{"subtype":"success","request_id":"…"}}
//     followed by a result frame with subtype "error_during_execution";
//     the process stays alive and accepts further turns.
//   - `--session-id <id>` may only be used for a session that does not
//     exist yet ("Session ID … is already in use." otherwise); respawns and
//     resumes must use `--resume <id>`, which keeps the same session id.
//   - Approvals: with the (undocumented but stable) `--permission-prompt-tool
//     stdio` flag, every tool call that the permission flow does not resolve
//     on its own arrives as
//     {"type":"control_request","request_id":"…","request":{"subtype":"can_use_tool",…}}
//     and blocks the turn until we answer with
//     {"type":"control_response","response":{"subtype":"success","request_id":"…","response":{…}}}.
//     See ResolveApproval for the answer shapes and the empirical evidence.
package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
	"github.com/luthermonson/shipmates/internal/runtime/containment/none"
)

// defaultInterruptGrace is how long InterruptTurn waits for the protocol
// interrupt to end the turn before falling back to killing the process
// tree via the containment handle.
const defaultInterruptGrace = 5 * time.Second

// Runtime is the claude-backed runtime.Runtime. Zero value is not usable;
// call New.
type Runtime struct {
	// binary is the resolved `claude` executable path.
	binary string
	// defaultArgs are appended to every invocation (e.g. per-firm flags).
	defaultArgs []string

	// contain wraps every subprocess spawn so RSS/CPU are bounded per
	// session process. Defaults to the no-op watcher when unset.
	contain containment.Watcher
	// limits are the per-process budget applied via contain.Start(). Zero =
	// no bounds.
	limits containment.Limits

	// interruptGrace bounds InterruptTurn's wait for the in-band interrupt
	// before escalating to a containment-handle kill. Tests shrink it.
	interruptGrace time.Duration

	stream  chan runtime.Event
	stopFan chan struct{}
	stopped sync.Once
	// fanWG tracks in-flight readLoop goroutines so Close can drain them
	// before closing the stream channel exactly once.
	fanWG sync.WaitGroup

	// sessMu guards sessions, closed, approvals, AND every session's mutable
	// fields (turnActive, turnID, turnDone, proc, resume). One lock keeps the
	// SendTurn/SteerTurn/InterruptTurn/ResolveApproval/Close interleavings
	// easy to reason about.
	sessMu   sync.Mutex
	closed   bool
	sessions map[string]*session
	// approvals holds every unanswered can_use_tool request, keyed by the
	// claude-assigned request_id. Entries are registered by readLoop before
	// the event reaches a consumer (so an immediate ResolveApproval always
	// finds one) and dropped on answer, turn end, or session teardown.
	approvals map[string]*pendingApproval
}

// pendingApproval is one unanswered can_use_tool request, plus everything
// needed to answer it: the process to write to, the turn to attribute it
// to, and the original tool input (which an "allow" must echo back).
type pendingApproval struct {
	sessionID string
	turnID    string
	proc      *proc
	request   ApprovalRequest
}

// Config controls claude runtime construction.
type Config struct {
	// Binary overrides the default "claude" name if the CLI isn't on PATH
	// or the operator wants a specific version.
	Binary string
	// DefaultArgs appear on every claude invocation after the session +
	// stream-format flags.
	DefaultArgs []string
	// Containment is the process-containment mode for spawned session
	// processes. nil defaults to none (no-op watcher). Plug in
	// containment/watchdog.New() or a cgroup Watcher to bound memory/CPU.
	Containment containment.Watcher
	// Limits are applied by Containment on every spawn. Zero-value means no
	// bounds.
	Limits containment.Limits
}

// New returns a claude runtime. It resolves the binary once; if the CLI is
// missing, subsequent operations fail on-demand rather than at New time,
// so the runtime is safe to construct even in shipmates commands that
// don't ultimately launch a session.
func New(cfg Config) *Runtime {
	binary := cfg.Binary
	if binary == "" {
		binary = "claude"
	}
	contain := cfg.Containment
	if contain == nil {
		contain = none.New()
	}
	return &Runtime{
		binary:         binary,
		defaultArgs:    append([]string(nil), cfg.DefaultArgs...),
		contain:        contain,
		limits:         cfg.Limits,
		interruptGrace: defaultInterruptGrace,
		stream:         make(chan runtime.Event, 64),
		stopFan:        make(chan struct{}),
		sessions:       map[string]*session{},
		approvals:      map[string]*pendingApproval{},
	}
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "claude" }

// Capabilities implements runtime.Runtime. With the persistent stream-json
// transport, Claude Code supports both mid-turn steering (additional user
// messages on stdin) and in-band interrupt (control_request). Containment
// here means the codex-adaptation cgroup launcher, which claude doesn't
// use; process bounds are applied shipmates-side via the containment
// watcher instead.
func (r *Runtime) Capabilities() runtime.Caps {
	return runtime.Caps{
		Streaming:   true,
		Interrupt:   true,
		Steer:       true,
		Attachments: true,
		Refusal:     false, // claude surfaces refusals as regular text events
		Containment: false,
		Environment: true, // SessionSpec.Environment applied to every spawn
		Approvals:   true, // can_use_tool control protocol, see ResolveApproval
	}
}

// Events implements runtime.Runtime.
func (r *Runtime) Events() <-chan runtime.Event { return r.stream }

// StartSession implements runtime.Runtime. The session's process is spawned
// lazily on the first SendTurn; "starting" a session just mints a fresh ID
// and remembers the persona/project binding.
func (r *Runtime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	id, err := mintSessionID()
	if err != nil {
		return nil, err
	}
	return r.rememberSession(id, spec, false), nil
}

// ResumeSession implements runtime.Runtime. A resumable claude session is
// identified by an id the caller previously received from StartSession
// (persisted in shipmates state). The next spawn uses `--resume <id>` —
// `--session-id` rejects ids that already exist on disk.
func (r *Runtime) ResumeSession(_ context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	if id == "" {
		return nil, errors.New("claude: empty session id")
	}
	return r.rememberSession(id, spec, true), nil
}

// CloseSession implements runtime.Runtime. Drops shipmates-side bookkeeping,
// closes the session process's stdin (letting claude exit cleanly), and
// tears down the containment handle. Does not delete the persisted
// transcript — the session id remains resumable.
func (r *Runtime) CloseSession(ctx context.Context, id string) error {
	r.sessMu.Lock()
	s, ok := r.sessions[id]
	var p *proc
	if ok {
		delete(r.sessions, id)
		p = s.proc
	}
	r.dropSessionApprovalsLocked(id)
	r.sessMu.Unlock()
	if p != nil {
		p.closeStdin()
		_ = p.handle.Close(ctx)
	}
	return nil
}

// SendTurn implements runtime.Runtime. Ensures the session's persistent
// `claude` process is running (spawning it on first use), writes the turn's
// user message to its stdin, and lets the session's readLoop stream JSONL
// events into r.stream until the per-turn result frame ends the turn.
func (r *Runtime) SendTurn(ctx context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	turnID, err := mintTurnID()
	if err != nil {
		return nil, err
	}

	// Reserve the session's single turn slot atomically. Claude Code
	// sessions are one-turn-at-a-time; a second user message while a turn
	// is in flight would be folded into it (that's SteerTurn's job), so a
	// concurrent SendTurn is an error. Reserving before the (lock-free)
	// spawn also guarantees at most one goroutine spawns the process.
	r.sessMu.Lock()
	if r.closed {
		r.sessMu.Unlock()
		return nil, fmt.Errorf("claude: runtime closed")
	}
	s, ok := r.sessions[sessionID]
	if !ok {
		r.sessMu.Unlock()
		return nil, fmt.Errorf("claude: unknown session %q", sessionID)
	}
	if s.turnActive {
		r.sessMu.Unlock()
		return nil, fmt.Errorf("claude: session %s already has an active turn", sessionID)
	}
	s.turnActive = true
	s.turnID = turnID
	s.turnDone = make(chan struct{})
	p := s.proc
	r.sessMu.Unlock()

	releaseSlot := func() {
		r.sessMu.Lock()
		if s.turnID == turnID && s.turnActive {
			s.turnActive = false
			s.turnID = ""
			close(s.turnDone)
		}
		r.sessMu.Unlock()
	}

	if p == nil {
		p, err = r.spawn(ctx, s)
		if err != nil {
			releaseSlot()
			return nil, err
		}
	}
	if err := p.writeUserMessage(in.Text, in.Attachments); err != nil {
		releaseSlot()
		return nil, fmt.Errorf("claude: write turn to session process: %w", err)
	}
	return &turn{id: turnID, sessionID: sessionID, startedAt: time.Now()}, nil
}

// SendTurnAttachments is SteerTurn with attachments: it folds an additional
// user message carrying image content blocks into the session's in-flight
// turn. Verified against claude 2.1.153 — an image block written mid-turn
// is absorbed by the running turn and answered in its single result frame,
// exactly like a text steer.
func (r *Runtime) SteerTurnInput(_ context.Context, sessionID, _ string, in runtime.TurnInput) error {
	r.sessMu.Lock()
	s, ok := r.sessions[sessionID]
	if !ok {
		r.sessMu.Unlock()
		return fmt.Errorf("claude: unknown session %q", sessionID)
	}
	if !s.turnActive || s.proc == nil {
		r.sessMu.Unlock()
		return fmt.Errorf("claude: session %s has no active turn to steer", sessionID)
	}
	p := s.proc
	r.sessMu.Unlock()
	if err := p.writeUserMessage(in.Text, in.Attachments); err != nil {
		return fmt.Errorf("claude: write steer to session process: %w", err)
	}
	return nil
}

// SteerTurn implements runtime.Runtime. Writes an additional user message
// to the session process's stdin while a turn is in flight; Claude Code
// folds it into the active turn (single result frame). Errors if the
// session has no active turn — a message written between turns would start
// a NEW turn, which is SendTurn's job.
func (r *Runtime) SteerTurn(ctx context.Context, sessionID, turnID string, text string) error {
	return r.SteerTurnInput(ctx, sessionID, turnID, runtime.TurnInput{Text: text})
}

// InterruptTurn implements runtime.Runtime. Sends the in-band interrupt
// (`control_request` subtype "interrupt") over the session process's stdin
// and waits up to interruptGrace for the turn's result frame; if the
// protocol interrupt doesn't land in time (or stdin is already broken), it
// falls back to killing the process tree via the containment handle.
func (r *Runtime) InterruptTurn(ctx context.Context, sessionID, _ string) error {
	r.sessMu.Lock()
	s, ok := r.sessions[sessionID]
	if !ok || !s.turnActive || s.proc == nil {
		r.sessMu.Unlock()
		return &runtime.ErrUnsupported{Runtime: "claude", Feature: "InterruptTurn on inactive session"}
	}
	p := s.proc
	done := s.turnDone
	r.sessMu.Unlock()

	reqID, err := mintTurnID()
	if err != nil {
		return err
	}
	if err := p.writeControlRequest(reqID, "interrupt"); err != nil {
		// Stdin is broken — the process is dying or dead; make sure the
		// tree is gone.
		return p.handle.Close(ctx)
	}
	grace := r.interruptGrace
	if grace <= 0 {
		grace = defaultInterruptGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-done:
		// Turn ended — the interrupt's result frame (subtype
		// error_during_execution) was emitted by readLoop. Process stays
		// alive for the next turn.
		return nil
	case <-timer.C:
		return p.handle.Close(ctx)
	case <-ctx.Done():
		return p.handle.Close(ctx)
	}
}

// ResolveApproval implements runtime.Runtime. It answers one pending
// can_use_tool request on the session process's stdin and reports whether
// the answer was written.
//
// Wire shapes, verified against claude 2.1.153 by driving the real binary
// (see docs/runtime-interface-plan.md for the transcript):
//
//	allow: {"type":"control_response","response":{"subtype":"success",
//	        "request_id":"…","response":{"behavior":"allow","updatedInput":{…}}}}
//	deny:  {"type":"control_response","response":{"subtype":"success",
//	        "request_id":"…","response":{"behavior":"deny","message":"…"}}}
//
// `updatedInput` is mandatory on allow for this version — an allow without
// it is rejected as a validation error and the tool call is denied — so the
// original tool input is echoed back verbatim. Shipmates never echoes
// `permission_suggestions` back in `updatedPermissions`: decisions are
// scoped to this one call, never persisted into the project's settings.
//
// An unknown, already-answered or misattributed request id is an error
// rather than a silent success, so the mediation layer fails closed instead
// of believing an answer landed. A decision of anything other than allow is
// written as a deny; there is no third state on the wire.
func (r *Runtime) ResolveApproval(ctx context.Context, resp runtime.ApprovalResponse, d runtime.ApprovalDecision) (bool, error) {
	if resp.ID == "" {
		return false, fmt.Errorf("claude: approval response carries no request id")
	}
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	default:
	}
	r.sessMu.Lock()
	p, ok := r.approvals[resp.ID]
	if !ok {
		r.sessMu.Unlock()
		return false, fmt.Errorf("claude: no pending approval %q", resp.ID)
	}
	// Identity is checked only when the caller supplied it, so a mediator
	// that tracks the tuple gets the correlation guarantee and one that
	// doesn't is still able to answer.
	if (resp.SessionID != "" && resp.SessionID != p.sessionID) || (resp.TurnID != "" && resp.TurnID != p.turnID) {
		r.sessMu.Unlock()
		return false, fmt.Errorf("claude: approval %q belongs to session %q turn %q", resp.ID, p.sessionID, p.turnID)
	}
	delete(r.approvals, resp.ID)
	r.sessMu.Unlock()

	body := map[string]any{"behavior": "deny", "message": denyMessage(d)}
	if d.Allow {
		input := p.request.InputJSON
		if len(input) == 0 {
			input = json.RawMessage(`{}`)
		}
		body = map[string]any{"behavior": "allow", "updatedInput": input}
	}
	if err := p.proc.writeControlResponse(resp.ID, body); err != nil {
		return false, fmt.Errorf("claude: write approval response: %w", err)
	}
	return true, nil
}

// denyMessage is what the model sees when a tool call is refused. Claude
// Code requires a non-empty message on deny and shows it to the model, so
// the operator's rationale is passed through when there is one.
func denyMessage(d runtime.ApprovalDecision) string {
	if s := strings.TrimSpace(d.Rationale); s != "" {
		return s
	}
	return "Denied by shipmates approval mediation."
}

// dropSessionApprovalsLocked forgets every pending approval belonging to a
// session. Answering them is pointless once their turn or their process is
// gone — claude has stopped waiting — and keeping them would let a late
// ResolveApproval claim success for a write nobody reads. Caller must hold
// sessMu.
func (r *Runtime) dropSessionApprovalsLocked(sessionID string) {
	for id, p := range r.approvals {
		if p.sessionID == sessionID {
			delete(r.approvals, id)
		}
	}
}

// Close implements runtime.Runtime. Tears down every open session's
// process (stdin close + containment handle) so the spawned trees exit,
// drains the reader goroutines, and closes the event stream so consumers
// ranging over Events() terminate — symmetric with the codex runtime's
// fanout.
func (r *Runtime) Close(ctx context.Context) error {
	r.stopped.Do(func() {
		close(r.stopFan)
		r.sessMu.Lock()
		r.closed = true
		var procs []*proc
		for _, s := range r.sessions {
			if s.proc != nil {
				procs = append(procs, s.proc)
			}
		}
		r.sessions = map[string]*session{}
		r.approvals = map[string]*pendingApproval{}
		r.sessMu.Unlock()
		for _, p := range procs {
			p.closeStdin()
			_ = p.handle.Close(ctx)
		}
		// Every readLoop goroutine bails out promptly once stopFan is
		// closed and its process is dead; wait for them, then close the
		// stream exactly once.
		r.fanWG.Wait()
		close(r.stream)
	})
	return nil
}

func (r *Runtime) rememberSession(id string, spec runtime.SessionSpec, resume bool) *session {
	r.sessMu.Lock()
	s := &session{
		id:          id,
		persona:     spec.Persona,
		projectDir:  spec.ProjectDir,
		wdOverride:  spec.WorkingDir,
		environment: spec.Environment,
		resume:      resume,
	}
	r.sessions[id] = s
	r.sessMu.Unlock()
	return s
}

// spawn launches the session's persistent claude process through the
// containment watcher and starts its readLoop. Caller must hold the
// session's turn slot (turnActive) so at most one spawn races per session.
func (r *Runtime) spawn(ctx context.Context, s *session) (*proc, error) {
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		// Route every unresolved tool permission to us as a can_use_tool
		// control_request instead of letting Claude Code decide alone. Without
		// it the persona's prompts are invisible to shipmates and the
		// operator can neither see nor answer them.
		"--permission-prompt-tool", "stdio",
	}
	r.sessMu.Lock()
	resume := s.resume
	r.sessMu.Unlock()
	if resume {
		// The session already exists on disk (prior process, or resumed
		// from persisted shipmates state): --session-id would fail with
		// "Session ID … is already in use"; --resume keeps the same id.
		args = append(args, "--resume", s.id)
	} else {
		args = append(args, "--session-id", s.id)
	}
	args = append(args, r.defaultArgs...)
	// Persona → --agent is the Claude Code convention shipmates uses.
	if s.persona != "" {
		args = append(args, "--agent", s.persona)
	}

	// Deliberately NOT CommandContext: the process is session-scoped and
	// must outlive any single turn's ctx. Teardown happens via the
	// containment handle (InterruptTurn fallback, CloseSession, Close).
	cmd := exec.Command(r.binary, args...)
	cmd.Dir = s.workingDir()
	// Child environment: inherit ours, export the persona so the
	// SessionStart hook (`shipmates hook load-memory`, wired by
	// InstallMemoryHook) can resolve which persona's memory to inject —
	// Claude Code doesn't pass the agent name to hooks — then layer on the
	// caller's SessionSpec.Environment overrides (sorted for determinism;
	// later entries win in exec.Cmd, so overrides beat inherited values).
	if s.persona != "" || len(s.environment) > 0 {
		childEnv := os.Environ()
		if s.persona != "" {
			childEnv = append(childEnv, "SHIPMATES_PERSONA="+s.persona)
		}
		keys := make([]string, 0, len(s.environment))
		for k := range s.environment {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			childEnv = append(childEnv, k+"="+s.environment[k])
		}
		cmd.Env = childEnv
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Route the spawn through containment so RSS/CPU are bounded.
	handle, err := r.contain.Start(cmd, r.limits)
	if err != nil {
		return nil, err
	}
	p := &proc{handle: handle, stdin: stdin}

	r.sessMu.Lock()
	if r.closed {
		// Runtime shut down while we were spawning: tear the process back
		// down rather than leaking it past Close's drain.
		r.sessMu.Unlock()
		p.closeStdin()
		_ = handle.Close(ctx)
		return nil, fmt.Errorf("claude: runtime closed")
	}
	s.proc = p
	// Any future spawn for this id must resume: the id now exists on disk.
	s.resume = true
	// Register with the fan-out drain inside the lock so Close cannot start
	// waiting between our closed-check and the Add.
	r.fanWG.Add(1)
	r.sessMu.Unlock()

	go func() {
		defer r.fanWG.Done()
		r.readLoop(s, p, stdout)
	}()
	return p, nil
}

// readLoop streams the session process's JSONL frames into r.stream for
// the process's whole lifetime. `result` frames end the active turn
// (releasing the turn slot and signalling turnDone); process death with a
// turn in flight surfaces the containment handle's terminal event as a
// KindError so consumers learn WHY the turn ended (memory kill, requested
// close, crash) rather than only that it did.
func (r *Runtime) readLoop(s *session, p *proc, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		isResult := frameType(line) == "result"
		r.sessMu.Lock()
		turnID := s.turnID
		if isResult && s.turnActive {
			s.turnActive = false
			s.turnID = ""
			close(s.turnDone)
			r.dropSessionApprovalsLocked(s.id)
		}
		r.sessMu.Unlock()
		ev := decodeFrame(line, s.id, turnID)
		if req, ok := ev.Payload.(ApprovalRequest); ok && ev.Kind == runtime.KindApprovalNeeded {
			// Register before publishing so a consumer that answers
			// synchronously always finds the pending entry.
			r.sessMu.Lock()
			if r.closed {
				r.sessMu.Unlock()
				return
			}
			r.approvals[req.RequestID] = &pendingApproval{sessionID: s.id, turnID: turnID, proc: p, request: req}
			r.sessMu.Unlock()
		}
		select {
		case <-r.stopFan:
			return
		case r.stream <- ev:
		}
	}
	// Stdout closed: the process is exiting. Wait for containment to
	// report the terminal event, then clean up session state.
	ev, ok := <-p.handle.Done()
	r.sessMu.Lock()
	if s.proc == p {
		s.proc = nil
	}
	turnID := s.turnID
	active := s.turnActive
	if active {
		s.turnActive = false
		s.turnID = ""
		close(s.turnDone)
	}
	r.dropSessionApprovalsLocked(s.id)
	r.sessMu.Unlock()

	kind := runtime.KindSessionClosed
	var payload any = ev
	if !ok {
		payload = "handle closed without terminal event"
	}
	if active {
		// The process died mid-turn — whatever the reason (memory kill,
		// crash, requested close, even a clean exit), the turn never got
		// its result frame, so surface it as an error terminal.
		kind = runtime.KindError
	}
	select {
	case <-r.stopFan:
	case r.stream <- runtime.Event{
		Timestamp: time.Now(),
		Kind:      kind,
		SessionID: s.id,
		TurnID:    turnID,
		Payload:   payload,
	}:
	}
}

// session is the runtime.Session implementation for claude sessions. The
// mutable fields (turnActive, turnID, turnDone, proc, resume) are guarded
// by Runtime.sessMu — see the Runtime struct comment.
type session struct {
	id, persona, projectDir, wdOverride string
	// environment holds SessionSpec.Environment overrides applied to the
	// session's spawned process. Immutable after rememberSession.
	environment map[string]string
	// resume selects `--resume` over `--session-id` for the next spawn:
	// set by ResumeSession and after every spawn (the id then exists in
	// Claude Code's on-disk session store).
	resume bool
	// turnActive reserves the session's single turn slot from the moment
	// SendTurn accepts a turn until the turn's result frame (or process
	// death) releases it.
	turnActive bool
	// turnID identifies the active turn for event attribution.
	turnID string
	// turnDone is closed when the active turn ends; InterruptTurn waits on
	// it. Recreated by each SendTurn.
	turnDone chan struct{}
	// proc is the session's live persistent process, nil before the first
	// SendTurn and after process death.
	proc *proc
}

func (s *session) ID() string      { return s.id }
func (s *session) Persona() string { return s.persona }
func (s *session) ProjectDir() string {
	return s.projectDir
}
func (s *session) workingDir() string {
	if s.wdOverride != "" {
		return s.wdOverride
	}
	return s.projectDir
}

// proc bundles a session's persistent claude process: the containment
// handle and the stream-json stdin.
type proc struct {
	handle containment.Handle
	stdin  io.WriteCloser
	// writeMu serializes stdin writers (SendTurn vs SteerTurn vs
	// InterruptTurn) so JSONL lines never interleave.
	writeMu   sync.Mutex
	stdinDone bool
}

// writeUserMessage writes one stream-json user-message line. With no
// attachments that is a single text block:
//
//	{"type":"user","message":{"role":"user","content":[{"type":"text","text":"…"}]}}
//
// With image attachments the image blocks precede the text block, using the
// Anthropic content-block shape (verified against claude 2.1.153):
//
//	{"type":"image","source":{"type":"base64","media_type":"image/png","data":"<b64>"}}
//
// Images come first because a trailing text block reads as the operator's
// instruction about the images that precede it.
func (p *proc) writeUserMessage(text string, attachments []runtime.Attachment) error {
	content := make([]map[string]any, 0, len(attachments)+1)
	for _, a := range attachments {
		if a.Kind != "image" || a.MediaType == "" || a.Base64 == "" {
			// Non-image attachments are represented in the turn text by
			// internal/attach; nothing to encode here. Refuse rather than
			// silently drop an image we cannot type.
			return fmt.Errorf("claude: attachment %q is not an encodable image content block", a.DisplayPath)
		}
		content = append(content, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": a.MediaType,
				"data":       a.Base64,
			},
		})
	}
	content = append(content, map[string]any{"type": "text", "text": text})
	msg := map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": content,
		},
	}
	return p.writeLine(msg)
}

// writeControlRequest writes one control_request line, e.g. subtype
// "interrupt":
//
//	{"type":"control_request","request_id":"…","request":{"subtype":"interrupt"}}
func (p *proc) writeControlRequest(requestID, subtype string) error {
	msg := map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    map[string]any{"subtype": subtype},
	}
	return p.writeLine(msg)
}

// writeControlResponse answers one control_request. `body` is the
// subtype-specific payload — for can_use_tool that is
// {"behavior":"allow","updatedInput":{…}} or
// {"behavior":"deny","message":"…"}:
//
//	{"type":"control_response","response":{"subtype":"success","request_id":"…","response":{…}}}
func (p *proc) writeControlResponse(requestID string, body any) error {
	return p.writeLine(map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   body,
		},
	})
}

func (p *proc) writeLine(msg any) error {
	buf, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if p.stdinDone {
		return errors.New("stdin closed")
	}
	_, err = p.stdin.Write(buf)
	return err
}

// closeStdin closes the process's stdin exactly once, letting claude exit
// cleanly on its own before the containment handle escalates.
func (p *proc) closeStdin() {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	if !p.stdinDone {
		p.stdinDone = true
		_ = p.stdin.Close()
	}
}

// turn is the runtime.Turn implementation for claude turns.
type turn struct {
	id, sessionID string
	startedAt     time.Time
}

func (t *turn) ID() string           { return t.id }
func (t *turn) SessionID() string    { return t.sessionID }
func (t *turn) StartedAt() time.Time { return t.startedAt }

// Compile-time assertion that Runtime satisfies runtime.Runtime.
var _ runtime.Runtime = (*Runtime)(nil)
