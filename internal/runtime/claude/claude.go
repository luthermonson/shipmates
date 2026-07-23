// Package claude implements runtime.Runtime by driving the `claude` CLI
// (Anthropic Claude Code) via non-interactive stdio streaming.
//
// Sessions map to `--session-id <uuid>` invocations of `claude -p
// --output-format stream-json`. Each SendTurn spawns (or continues) a
// session, writes the turn message to stdin, and streams parsed JSONL
// events into the runtime.Event channel. The session state (recent turn
// IDs, running processes) lives in Runtime.
//
// # Phase 2 status
//
// This is the initial cross-runtime Claude implementation restored after
// the codex-adaptation branch removed the fleet-side Claude integration.
// It's intentionally minimal: single-turn invocations, no PTY, no
// interactive multi-turn conversations, no persona-side hooks. Follow-ups:
//
//   - Session persistence across restarts (Claude Code already stores by
//     session-id under ~/.claude; we just need to remember which id
//     belongs to which shipmates persona)
//   - PTY-hosted interactive mates — the on-main story from
//     internal/server/ptyproc.go. Reintroduce alongside the runtime
//     wrapper as an "attached mode" transport.
//   - Attachments: Claude Code accepts image files via the -f flag or an
//     inline content JSON. Wire once the persona catalog format lands.
package claude

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
	"github.com/luthermonson/shipmates/internal/runtime/containment/none"
)

// Runtime is the claude-backed runtime.Runtime. Zero value is not usable;
// call New.
type Runtime struct {
	// binary is the resolved `claude` executable path.
	binary string
	// defaultArgs are appended to every invocation (e.g. per-firm flags).
	defaultArgs []string

	// contain wraps every subprocess spawn so RSS/CPU are bounded per turn.
	// Defaults to the no-op watcher when unset.
	contain containment.Watcher
	// limits are the per-turn budget applied via contain.Start(). Zero =
	// no bounds.
	limits containment.Limits

	stream  chan runtime.Event
	stopFan chan struct{}
	stopped sync.Once

	sessMu   sync.Mutex
	sessions map[string]*session
}

// Config controls claude runtime construction.
type Config struct {
	// Binary overrides the default "claude" name if the CLI isn't on PATH
	// or the operator wants a specific version.
	Binary string
	// DefaultArgs appear on every claude invocation before the turn
	// message, after the session-id + output-format flags.
	DefaultArgs []string
	// Containment is the process-containment mode for spawned turns.
	// nil defaults to none (no-op watcher). Plug in
	// containment/watchdog.New() or a cgroup Watcher to bound memory/CPU.
	Containment containment.Watcher
	// Limits are applied by Containment on every turn spawn. Zero-value
	// means no bounds.
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
		binary:      binary,
		defaultArgs: append([]string(nil), cfg.DefaultArgs...),
		contain:     contain,
		limits:      cfg.Limits,
		stream:      make(chan runtime.Event, 64),
		stopFan:     make(chan struct{}),
		sessions:    map[string]*session{},
	}
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return "claude" }

// Capabilities implements runtime.Runtime. Claude Code streams events over
// JSONL, doesn't currently expose a mid-turn steer or interrupt, and its
// containment story on Linux uses shipmates-side sandboxing (not the
// codex-adaptation cgroup launcher).
func (r *Runtime) Capabilities() runtime.Caps {
	return runtime.Caps{
		Streaming:   true,
		Interrupt:   false,
		Steer:       false,
		Attachments: true,
		Refusal:     false, // claude surfaces refusals as regular text events
		Containment: false,
	}
}

// Events implements runtime.Runtime.
func (r *Runtime) Events() <-chan runtime.Event { return r.stream }

// StartSession implements runtime.Runtime. Claude Code sessions are
// content-addressed by session-id, so "starting" a session just means
// minting a fresh ID and remembering the persona/project binding.
func (r *Runtime) StartSession(_ context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	id, err := mintSessionID()
	if err != nil {
		return nil, err
	}
	return r.rememberSession(id, spec), nil
}

// ResumeSession implements runtime.Runtime. A resumable claude session is
// identified by an id the caller previously received from StartSession
// (persisted in shipmates state); we validate the id shape and re-bind it
// to spec.Persona / spec.ProjectDir.
func (r *Runtime) ResumeSession(_ context.Context, id string, spec runtime.SessionSpec) (runtime.Session, error) {
	if id == "" {
		return nil, errors.New("claude: empty session id")
	}
	return r.rememberSession(id, spec), nil
}

// CloseSession implements runtime.Runtime. Drops shipmates-side bookkeeping
// and interrupts any turn currently running under the session.
func (r *Runtime) CloseSession(ctx context.Context, id string) error {
	r.sessMu.Lock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
	}
	r.sessMu.Unlock()
	if ok && s.currentHandle != nil {
		_ = s.currentHandle.Close(ctx)
	}
	return nil
}

// SendTurn implements runtime.Runtime. Spawns `claude -p --session-id <id>
// --output-format stream-json`, writes the turn text to stdin, and streams
// JSONL events into r.stream via fanoutTurn.
func (r *Runtime) SendTurn(ctx context.Context, sessionID string, in runtime.TurnInput) (runtime.Turn, error) {
	r.sessMu.Lock()
	s, ok := r.sessions[sessionID]
	r.sessMu.Unlock()
	if !ok {
		return nil, fmt.Errorf("claude: unknown session %q", sessionID)
	}
	turnID, err := mintTurnID()
	if err != nil {
		return nil, err
	}

	args := []string{
		"-p",
		"--session-id", sessionID,
		"--output-format", "stream-json",
	}
	args = append(args, r.defaultArgs...)
	// Persona → --agent is the Claude Code convention shipmates uses.
	if s.persona != "" {
		args = append(args, "--agent", s.persona)
	}

	cmd := exec.CommandContext(ctx, r.binary, args...)
	cmd.Dir = s.workingDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Route the spawn through containment so RSS/CPU are bounded per turn.
	handle, err := r.contain.Start(cmd, r.limits)
	if err != nil {
		return nil, err
	}
	s.currentCmd = cmd
	s.currentHandle = handle

	// Write the turn message and close stdin so claude knows we're done.
	go func() {
		defer stdin.Close()
		_, _ = io.WriteString(stdin, in.Text)
	}()

	// Fan out events from claude's JSONL output into r.stream; the
	// Handle's Done() supplies the terminal event (natural exit, memory
	// kill, requested close).
	go r.fanoutTurn(sessionID, turnID, handle, stdout)

	return &turn{id: turnID, sessionID: sessionID, startedAt: time.Now()}, nil
}

// InterruptTurn implements runtime.Runtime. Claude Code has no in-band
// interrupt; we tear down the containment handle so the whole process
// tree dies atomically (Job Object on Windows, process group on Unix).
func (r *Runtime) InterruptTurn(ctx context.Context, sessionID, _ string) error {
	r.sessMu.Lock()
	s, ok := r.sessions[sessionID]
	r.sessMu.Unlock()
	if !ok || s.currentHandle == nil {
		return &runtime.ErrUnsupported{Runtime: "claude", Feature: "InterruptTurn on inactive session"}
	}
	return s.currentHandle.Close(ctx)
}

// SteerTurn implements runtime.Runtime. Claude Code has no equivalent of
// mid-turn steering, so this returns ErrUnsupported. Consumers can achieve
// similar UX by interrupting and re-sending an amended turn.
func (r *Runtime) SteerTurn(context.Context, string, string, string) error {
	return &runtime.ErrUnsupported{Runtime: "claude", Feature: "SteerTurn"}
}

// ResolveApproval implements runtime.Runtime. Claude Code approvals arrive
// through the SessionStart / PermissionRequest hook shipmates already
// wires; that plumbing hooks in via InstallMemoryHook + a companion
// permissions hook in later phases. Until then, we return
// ErrUnsupported so callers know they must not rely on it.
func (r *Runtime) ResolveApproval(context.Context, runtime.ApprovalResponse, runtime.ApprovalDecision) (bool, error) {
	return false, &runtime.ErrUnsupported{Runtime: "claude", Feature: "ResolveApproval (Phase 4 hook plumbing)"}
}

// Close implements runtime.Runtime. Tears down every open session's
// containment handle so the whole spawned process tree exits.
func (r *Runtime) Close(ctx context.Context) error {
	r.stopped.Do(func() { close(r.stopFan) })
	r.sessMu.Lock()
	for _, s := range r.sessions {
		if s.currentHandle != nil {
			_ = s.currentHandle.Close(ctx)
		}
	}
	r.sessions = map[string]*session{}
	r.sessMu.Unlock()
	return nil
}

func (r *Runtime) rememberSession(id string, spec runtime.SessionSpec) *session {
	r.sessMu.Lock()
	s := &session{
		id:         id,
		persona:    spec.Persona,
		projectDir: spec.ProjectDir,
		wdOverride: spec.WorkingDir,
	}
	r.sessions[id] = s
	r.sessMu.Unlock()
	return s
}

// fanoutTurn reads JSONL frames from claude's stdout, decodes each via
// decodeFrame (see events.go), and streams normalized runtime.Events.
// The containment handle's Done() channel supplies the terminal event
// so consumers learn WHY the turn ended (natural exit, memory_limit,
// requested close, etc.) rather than only that it did.
func (r *Runtime) fanoutTurn(sessionID, turnID string, handle containment.Handle, stdout io.Reader) {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		select {
		case <-r.stopFan:
			return
		case r.stream <- decodeFrame(scanner.Bytes(), sessionID, turnID):
		}
	}
	// Wait for containment to report the terminal event.
	ev, ok := <-handle.Done()
	kind := runtime.KindTurnDone
	var payload any = ev
	if !ok {
		kind = runtime.KindError
		payload = "handle closed without terminal event"
	} else {
		switch ev.Reason {
		case containment.ReasonExited:
			// natural exit — but non-zero exit code should surface as error
			if ev.ExitCode != 0 {
				kind = runtime.KindError
			}
		case containment.ReasonMemoryLimit, containment.ReasonCPULimit,
			containment.ReasonRequested, containment.ReasonStartFailed,
			containment.ReasonUnknown:
			kind = runtime.KindError
		}
	}
	select {
	case <-r.stopFan:
	case r.stream <- runtime.Event{
		Timestamp: time.Now(),
		Kind:      kind,
		SessionID: sessionID,
		TurnID:    turnID,
		Payload:   payload,
	}:
	}
}

// session is the runtime.Session implementation for claude sessions.
type session struct {
	id, persona, projectDir, wdOverride string
	currentCmd                          *exec.Cmd
	currentHandle                       containment.Handle
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
