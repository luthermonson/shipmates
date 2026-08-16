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
//
// Session processes are spawned through the Supervisor seam in supervise.go so
// this package carries no dependency on shipmates' containment implementation.
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
)

// defaultInterruptGrace is how long InterruptTurn waits for the protocol
// interrupt to end the turn before falling back to killing the process
// tree via the supervisor handle.
const defaultInterruptGrace = 5 * time.Second

// defaultMaxFrameBytes bounds one stdout JSONL frame. A frame this large is
// already pathological (claude's own frames are kilobytes), but the bound has
// to exist or a malformed stream could grow the read buffer without limit.
const defaultMaxFrameBytes = 8 * 1024 * 1024

// stderrTailBytes is how much of a session process's stderr is retained for
// diagnosis. The child's last words are what distinguish "please run /login",
// a missing binary and a crash from a generic timeout, and the interesting
// output is always at the end.
const stderrTailBytes = 16 * 1024

// readerDrainGrace bounds how long a dead child's stdout/stderr pipe read ends
// stay open before they are force-closed.
//
// The pipes are ours (os.Pipe, wired onto cmd.Stdout/cmd.Stderr) rather than
// cmd.StdoutPipe/cmd.StderrPipe on purpose. os/exec closes the pipes IT created
// the instant Wait reaps the child — "it is incorrect to call Wait before all
// reads from the pipe have completed", and the supervisor calls Wait on its own
// goroutine, concurrently with both readers. Losing that race discarded
// whatever was still buffered, which showed up two ways: the stderr tail came
// out empty, so a terminal event reported a bare exit code instead of "Invalid
// API key · Please run /login"; or the stdout scanner was aborted mid-read, so
// the terminal event was published through the framing-failure path as a
// *string* instead of a Terminal. Owning the pipes means only this package ever
// closes them, and it does so only once the readers are finished.
//
// The cost of owning them is that EOF now needs every writer gone, and a
// descendant that inherited the child's stdout can hold one open past the
// child's own death. This grace bounds that case and nothing else: once the
// process is reaped, draining what it already wrote takes microseconds, so
// reaching this deadline means somebody else still holds the descriptor.
const readerDrainGrace = 10 * time.Second

// stderrDrainBackstop bounds readLoop's wait for the stderr tail before it
// publishes a terminal event. It is a deadlock guard, not a policy: reapPipes
// closes the read end after readerDrainGrace, which ends the copy, so the only
// way to reach this is a proc that has no reaper (readLoop driven directly by a
// test). It must stay comfortably larger than readerDrainGrace.
const stderrDrainBackstop = readerDrainGrace + 5*time.Second

// Runtime is the claude-backed runtime.Runtime. Zero value is not usable;
// call New.
type Runtime struct {
	// binary is the resolved `claude` executable path.
	binary string
	// defaultArgs are appended to every invocation (e.g. per-firm flags).
	defaultArgs []string

	// supervise wraps every subprocess spawn so the process tree can be
	// bounded and torn down. Defaults to the direct (unbounded) supervisor.
	supervise Supervisor

	// interruptGrace bounds InterruptTurn's wait for the in-band interrupt
	// before escalating to a handle kill. Tests shrink it.
	interruptGrace time.Duration

	// maxFrameBytes is readLoop's per-frame ceiling. Tests shrink it so the
	// oversized-frame path is reachable without writing 8 MiB.
	maxFrameBytes int

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
	// Supervisor spawns session processes. nil means the direct supervisor:
	// the process runs unbounded and Close kills the top-level process only.
	// Bind shipmates' containment watcher here to get RSS/CPU bounds and
	// process-tree teardown — see the adapter sketch in supervise.go.
	Supervisor Supervisor
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
	sup := cfg.Supervisor
	if sup == nil {
		sup = directSupervisor{}
	}
	return &Runtime{
		binary:         binary,
		defaultArgs:    append([]string(nil), cfg.DefaultArgs...),
		supervise:      sup,
		interruptGrace: defaultInterruptGrace,
		maxFrameBytes:  defaultMaxFrameBytes,
		stream:         make(chan runtime.Event, 64),
		stopFan:        make(chan struct{}),
		sessions:       map[string]*session{},
		approvals:      map[string]*pendingApproval{},
	}
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "claude" }

// Capabilities implements runtime.Runtime. Every field below is what this
// package actually does, not what the CLI might be capable of:
//
//   - Streaming: readLoop publishes every stdout frame as it is scanned, so
//     text, tool calls and tool results arrive during the turn.
//   - Interrupt: InterruptTurn writes the in-band control_request and, if the
//     turn does not end within interruptGrace, kills the process through the
//     supervisor handle. Either way the turn is really over.
//   - Steer: SteerTurn writes an extra user message to the live process's
//     stdin, which Claude Code folds into the running turn.
//   - Attachments: writeUserMessage encodes image attachments as base64
//     content blocks. An attachment it cannot encode is an ERROR, never a
//     silent drop — that is what keeps this `true` honest.
//   - Refusal: false. Claude Code surfaces model refusals as ordinary
//     assistant text; there is no protocol-level refusal signal to
//     distinguish, so claiming otherwise would be a lie.
//   - Containment: false. This is specifically SessionSpec.ContainExec, which
//     this package never consults; process bounds are applied shipmates-side
//     by whichever Supervisor is configured, and the zero Config supervises
//     nothing. SendTurn ignores ContainExec, so reporting false is what tells
//     callers it is ignored.
//   - Environment: true. spawn applies SessionSpec.Environment to the child.
//   - Approvals: true. `--permission-prompt-tool stdio` routes unresolved
//     tool permissions to us as can_use_tool control_requests, which surface
//     as KindApprovalNeeded and are answered by ResolveApproval.
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
	if err := validateSpecPersona(spec); err != nil {
		return nil, err
	}
	id, err := mintSessionID()
	if err != nil {
		return nil, err
	}
	return r.rememberSession(id, spec, false), nil
}

// validateSpecPersona rejects persona names that could smuggle a path
// segment or an extra CLI flag: the name is passed to the child process as
// the --agent value and exported as SHIPMATES_PERSONA. Empty stays allowed —
// a session without a persona is legitimate.
func validateSpecPersona(spec runtime.SessionSpec) error {
	if spec.Persona == "" {
		return nil
	}
	if err := validatePersonaName(spec.Persona); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	return nil
}

// ResumeSession implements runtime.Runtime. A resumable claude session is
// identified by an id the caller previously received from StartSession
// (persisted in shipmates state). The next spawn uses `--resume <id>` —
// `--session-id` rejects ids that already exist on disk.
func (r *Runtime) ResumeSession(_ context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	if id == "" {
		return nil, errors.New("claude: empty session id")
	}
	if err := validateSpecPersona(spec); err != nil {
		return nil, err
	}
	return r.rememberSession(id, spec, true), nil
}

// CloseSession implements runtime.Runtime. Drops shipmates-side bookkeeping,
// closes the session process's stdin (letting claude exit cleanly), and
// tears down the supervisor handle. Does not delete the persisted
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
		// A broken stdin nearly always means the child already exited, and the
		// reason is on its stderr — "please run /login" rather than a bare
		// "file already closed". Keep %w so callers can still unwrap.
		if tail := p.lastStderr(); tail != "" {
			return nil, fmt.Errorf("claude: write turn to session process: %w; stderr: %s", err, tail)
		}
		return nil, fmt.Errorf("claude: write turn to session process: %w", err)
	}
	return &turn{id: turnID, sessionID: sessionID, startedAt: time.Now()}, nil
}

// SteerTurnInput is SteerTurn with attachments: it folds an additional
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
// falls back to killing the process tree via the supervisor handle.
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
// Wire shapes, verified against claude 2.1.153 by driving the real binary:
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
// process (stdin close + supervisor handle) so the spawned trees exit,
// drains the reader goroutines, and closes the event stream so consumers
// ranging over Events() terminate.
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
// supervisor and starts its readLoop. Caller must hold the session's turn
// slot (turnActive) so at most one spawn races per session.
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
	// Persona → --agent is the Claude Code convention shipmates uses. The name
	// was validated at StartSession/ResumeSession (validateSpecPersona), which
	// is what stops a leading dash from being read as another flag here.
	if s.persona != "" {
		if err := validatePersonaName(s.persona); err != nil {
			return nil, fmt.Errorf("claude: %w", err)
		}
		args = append(args, "--agent", s.persona)
	}

	// Deliberately NOT CommandContext: the process is session-scoped and
	// must outlive any single turn's ctx. Teardown happens via the
	// supervisor handle (InterruptTurn fallback, CloseSession, Close).
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
	// Both output pipes are opened here rather than through
	// cmd.StdoutPipe/cmd.StderrPipe so that os/exec's Wait cannot close them out
	// from under a reader that is still draining — see readerDrainGrace for the
	// two bugs that caused. reapPipes closes them instead, once the readers are
	// done. Claude Code reports auth failures ("Invalid API key · Please run
	// /login"), config errors and crash detail on stderr; left unwired it goes
	// to the void and every one of those failures looks like a generic timeout.
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeFiles(stdoutR, stdoutW)
		return nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	// Route the spawn through the supervisor so RSS/CPU can be bounded and the
	// tree torn down.
	handle, err := r.supervise.Start(cmd)
	if err != nil {
		closeFiles(stdoutR, stdoutW, stderrR, stderrW)
		return nil, err
	}
	// The child holds its own duplicates of the write ends now; ours have to go
	// or the read ends would never reach EOF.
	closeFiles(stdoutW, stderrW)
	p := newProc(handle, stdin, stdoutR, stderrR)
	go func() {
		defer close(p.stderrDone)
		// The read end is ours, so this copy ends at a clean EOF once the child
		// (and anything that inherited its stderr) is gone: every byte the child
		// wrote is in the tail before the terminal event is published. The tail
		// is a bounded ring whose Write never blocks, so a chatty child can
		// never wedge on a full pipe either.
		_, _ = io.Copy(p.stderr, stderrR)
	}()
	// The reaper is what eventually closes the read ends. Started before the
	// closed-runtime check below so no path can leak the descriptors.
	go r.reapPipes(p)

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
		r.readLoop(s, p, stdoutR)
	}()
	return p, nil
}

// closeFiles closes every non-nil file, ignoring errors. Used on the spawn
// error paths, where a half-wired set of pipes has to go back.
func closeFiles(files ...*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}

// reapPipes closes the child's pipe read ends — but never while a reader could
// still be mid-stream. That ordering IS the fix for the teardown race: os/exec
// used to own these pipes and closed them the instant it reaped the child,
// which either discarded unread stderr (a terminal event with no reason) or
// aborted the stdout scanner mid-read (a terminal event whose payload changed
// from Terminal to string). Nothing closes them here until both readers have
// finished, the drain backstop has expired on an already-dead process, or the
// runtime is shutting down.
func (r *Runtime) reapPipes(p *proc) {
	select {
	case <-p.termDone:
		// The process is gone. Anything still holding a write end is a
		// descendant that inherited it, not the child, so bound the wait.
		t := time.NewTimer(readerDrainGrace)
		defer t.Stop()
		waitClosed(p.stdoutDone, t.C, r.stopFan)
		waitClosed(p.stderrDone, t.C, r.stopFan)
	case <-r.stopFan:
		// Explicit shutdown: Close must not sit through a drain window.
	}
	p.closePipes()
}

// waitClosed blocks until c is closed, reporting whether it actually was.
// Expiring the deadline or aborting both mean "stop waiting", never "the wait
// succeeded" — the difference is what lets a caller say "no stderr" instead of
// silently implying the child said nothing.
func waitClosed(c <-chan struct{}, deadline <-chan time.Time, abort <-chan struct{}) bool {
	if c == nil {
		return false
	}
	select {
	case <-c:
		return true
	case <-deadline:
	case <-abort:
	}
	return false
}

// readLoop streams the session process's JSONL frames into r.stream for
// the process's whole lifetime. `result` frames end the active turn
// (releasing the turn slot and signalling turnDone); process death with a
// turn in flight surfaces the supervisor handle's terminal event as a
// KindError so consumers learn WHY the turn ended (memory kill, requested
// close, crash) rather than only that it did.
func (r *Runtime) readLoop(s *session, p *proc, stdout io.Reader) {
	// Whatever ends this goroutine, the stdout reader is finished with the pipe
	// once it returns; reapPipes must not have to sit out the whole drain grace
	// on the early-return paths below.
	defer p.stdoutFinished()
	scanner := bufio.NewScanner(stdout)
	max := r.maxFrameBytes
	if max <= 0 {
		max = defaultMaxFrameBytes
	}
	// bufio raises the token limit to cap(buf) when that is larger, so the
	// starting buffer must never exceed max or the ceiling silently moves.
	start := 64 * 1024
	if start > max {
		start = max
	}
	scanner.Buffer(make([]byte, 0, start), max)
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
		if !r.publish(ev) {
			return
		}
	}
	// scanner.Scan returning false is NOT proof of clean EOF: a frame longer
	// than the token limit (or any read fault) ends the loop with the child
	// still alive and still emitting frames we can no longer parse. Ignoring
	// scanner.Err made truncation indistinguishable from a clean exit and left
	// this goroutine waiting for a terminal event nobody would ever trigger,
	// which in turn hung Close on fanWG.Wait forever. Report the real cause and
	// tear the desynced process down.
	scanErr := scanner.Err()
	p.stdoutFinished()
	if isPipeClosed(scanErr) {
		// ...but a read that failed because the PIPE went away is teardown, not
		// a malformed stream: the descriptor is closed by reapPipes once the
		// child is reaped (and, before this package owned the pipes, by
		// os/exec's Wait, mid-frame). Reporting that as a framing failure made
		// the terminal event's payload type depend on which reader finished
		// first — a consumer type-switching on it silently took another branch —
		// so a closed pipe classifies as EOF, exactly like a clean exit. A
		// genuinely malformed stream still arrives as ErrTooLong or a real I/O
		// fault and is still reported below.
		scanErr = nil
	}
	if scanErr != nil {
		r.sessMu.Lock()
		turnID := s.turnID
		r.sessMu.Unlock()
		if !r.publish(runtime.Event{
			Timestamp: time.Now(),
			Kind:      runtime.KindError,
			SessionID: s.id,
			TurnID:    turnID,
			Payload:   withStderr(fmt.Sprintf("claude: session stdout framing failed: %v", scanErr), p.lastStderr()),
		}) {
			return
		}
		p.closeStdin()
		_ = p.handle.Close(context.Background())
	}
	// Stdout closed: the process is exiting. Wait for the supervisor to
	// report the terminal event, then clean up session state. The stopFan arm
	// is what guarantees Close's drain can never be stranded here, whatever
	// state the child or its handle is in.
	var ev Terminal
	ok := false
	select {
	case <-p.termDone:
		ev, ok = p.terminal()
	case <-r.stopFan:
	}
	drained := true
	if ok {
		// Wait for the stderr reader to finish, unconditionally. The pipe read
		// end belongs to this package and reapPipes closes it only after this
		// same drain window, so the copy is guaranteed to end and the child's
		// last words are guaranteed to be in the tail. The backstop below is a
		// deadlock guard, not a budget: the 2s budget it replaces is what let a
		// terminal event go out reporting a bare exit code.
		t := time.NewTimer(stderrDrainBackstop)
		drained = waitClosed(p.stderrDone, t.C, r.stopFan)
		t.Stop()
	}
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

	if scanErr != nil {
		// The framing failure was already published as this session's error
		// terminal; a second terminal event would double-report one death.
		return
	}

	kind := runtime.KindSessionClosed
	tail := stderrDiagnosis(p.lastStderr(), drained)
	var payload any = ev
	if !ok {
		payload = withStderr("handle closed without terminal event", tail)
	} else {
		// Terminal.Detail is the diagnosis slot: an exit code alone never
		// explains WHY claude refused to run.
		ev.Detail = withStderr(ev.Detail, tail)
		payload = ev
	}
	if active {
		// The process died mid-turn — whatever the reason (memory kill,
		// crash, requested close, even a clean exit), the turn never got
		// its result frame, so surface it as an error terminal.
		kind = runtime.KindError
	}
	r.publish(runtime.Event{
		Timestamp: time.Now(),
		Kind:      kind,
		SessionID: s.id,
		TurnID:    turnID,
		Payload:   payload,
	})
}

// publish sends one event on the fan-out stream, reporting false when the
// runtime is shutting down and the event was dropped.
func (r *Runtime) publish(ev runtime.Event) bool {
	select {
	case <-r.stopFan:
		return false
	case r.stream <- ev:
		return true
	}
}

// isPipeClosed reports whether a read failed because the descriptor went away
// rather than because the stream was malformed. os/exec's Wait and reapPipes
// both surface as *fs.PathError{Err: os.ErrClosed} ("read |0: file already
// closed"); an io.Pipe surfaces as io.ErrClosedPipe.
func isPipeClosed(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, os.ErrClosed) || errors.Is(err, io.ErrClosedPipe)
}

// stderrDiagnosis describes what was collected from the child's stderr. An
// operator has to be able to tell "the child said nothing" from "we never
// finished collecting what it said" — the two used to be spelled identically,
// which is how a dropped tail read as a silent child.
func stderrDiagnosis(tail string, drained bool) string {
	if drained {
		return tail
	}
	if tail == "" {
		return "stderr not collected: the pipe was still open at teardown"
	}
	return tail + " (stderr may be incomplete: the pipe was still open at teardown)"
}

// withStderr appends a captured stderr tail to a diagnostic string.
func withStderr(detail, tail string) string {
	if tail == "" {
		return detail
	}
	if detail == "" {
		return "stderr: " + tail
	}
	return detail + "; stderr: " + tail
}

// stderrTail is a bounded ring of a child's most recent stderr output. Writes
// never block and never grow past max, so the reader draining the child's
// stderr pipe can always keep up and the child can never wedge on a full pipe.
type stderrTail struct {
	mu       sync.Mutex
	buf      []byte
	max      int
	overflow bool
}

func newStderrTail(max int) *stderrTail {
	if max <= 0 {
		max = stderrTailBytes
	}
	return &stderrTail{max: max}
}

func (t *stderrTail) Write(p []byte) (int, error) {
	n := len(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) >= t.max {
		t.buf = append(t.buf[:0], p[len(p)-t.max:]...)
		t.overflow = true
		return n, nil
	}
	if drop := len(t.buf) + len(p) - t.max; drop > 0 {
		t.buf = t.buf[:copy(t.buf, t.buf[drop:])]
		t.overflow = true
	}
	t.buf = append(t.buf, p...)
	return n, nil
}

// String returns the retained tail with surrounding whitespace trimmed, marked
// with a leading ellipsis when earlier output was dropped.
func (t *stderrTail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := strings.TrimSpace(string(t.buf))
	if out == "" {
		return ""
	}
	if t.overflow {
		return "..." + out
	}
	return out
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

// proc bundles a session's persistent claude process: the supervisor handle,
// the stream-json stdin, and the two output pipes this package owns.
type proc struct {
	handle Handle
	stdin  io.WriteCloser
	// stdoutPipe and stderrPipe are the read ends of the child's output pipes.
	// They belong to this package rather than to os/exec (see
	// readerDrainGrace), so nothing can close them while a reader is
	// mid-stream; reapPipes closes them. nil in tests that drive readLoop over
	// a plain io.Reader.
	stdoutPipe, stderrPipe *os.File
	// stderr retains the tail of the child's stderr so a failure that only
	// shows up there (auth, config, crash detail) reaches the operator instead
	// of looking like a generic timeout.
	stderr *stderrTail
	// stdoutDone closes when readLoop has finished with the stdout pipe,
	// stderrDone when the stderr copy has finished with its own. reapPipes
	// waits for both before closing either.
	stdoutDone chan struct{}
	stderrDone chan struct{}
	stdoutOnce sync.Once
	// termDone closes once the supervisor's terminal event has been consumed;
	// term/termOK hold it. Handle.Done delivers that event exactly once, so a
	// single consumer reads it and republishes it here — readLoop and
	// reapPipes both need to know the process died, and neither may steal it
	// from the other.
	termDone chan struct{}
	termMu   sync.Mutex
	term     Terminal
	termOK   bool
	pipeOnce sync.Once
	// writeMu serializes stdin writers (SendTurn vs SteerTurn vs
	// InterruptTurn) so JSONL lines never interleave.
	writeMu   sync.Mutex
	stdinDone bool
}

// newProc wires a started session process and begins consuming the handle's
// single terminal event. stdout/stderr are the read ends of the child's output
// pipes and may be nil in tests.
func newProc(handle Handle, stdin io.WriteCloser, stdout, stderr *os.File) *proc {
	p := &proc{
		handle:     handle,
		stdin:      stdin,
		stdoutPipe: stdout,
		stderrPipe: stderr,
		stderr:     newStderrTail(stderrTailBytes),
		stdoutDone: make(chan struct{}),
		stderrDone: make(chan struct{}),
		termDone:   make(chan struct{}),
	}
	go func() {
		defer close(p.termDone)
		ev, ok := <-handle.Done()
		p.termMu.Lock()
		p.term, p.termOK = ev, ok
		p.termMu.Unlock()
	}()
	return p
}

// terminal returns the supervisor's terminal event. Only valid once termDone
// is closed; ok is false when the handle closed without reporting one.
func (p *proc) terminal() (Terminal, bool) {
	p.termMu.Lock()
	defer p.termMu.Unlock()
	return p.term, p.termOK
}

// stdoutFinished records that no reader is touching the stdout pipe any more.
func (p *proc) stdoutFinished() {
	if p == nil || p.stdoutDone == nil {
		return
	}
	p.stdoutOnce.Do(func() { close(p.stdoutDone) })
}

// closePipes releases the child's pipe read ends. Only reapPipes calls it, and
// only once no reader can still be draining them.
func (p *proc) closePipes() {
	p.pipeOnce.Do(func() { closeFiles(p.stdoutPipe, p.stderrPipe) })
}

// lastStderr returns the child's retained stderr, or "" when nothing was
// captured. Safe on a proc built without stderr wiring (tests).
func (p *proc) lastStderr() string {
	if p == nil || p.stderr == nil {
		return ""
	}
	return p.stderr.String()
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
			// silently drop an image we cannot type — Caps.Attachments is
			// only honest if an attachment never disappears without a word.
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
// cleanly on its own before the supervisor handle escalates.
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
