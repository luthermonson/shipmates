// Package runtime is the shipmates AI-runtime abstraction. Implementations
// live under runtime/claude and runtime/codex; the rest of shipmates depends
// only on this interface. See docs/runtime-interface-plan.md for the full
// design and migration order.
package runtime

import (
	"context"
	"time"
)

// Runtime is a session-based, tool-using coding agent (Claude Code, Codex,
// or a future addition). It intentionally mirrors the shape of the existing
// codexapp.Adapter so the Codex refactor in Phase 1 is largely a signature
// rename.
type Runtime interface {
	// Name identifies the runtime for configuration and logging: "claude",
	// "codex", etc.
	Name() string

	// Capabilities reports what this runtime can do. Callers must check
	// before requesting optional features like Steer or Containment.
	Capabilities() Caps

	// StartSession begins a fresh persona session (Codex calls this a
	// "thread"). Returns a handle to the session.
	StartSession(ctx context.Context, spec SessionSpec) (Session, error)

	// ResumeSession attaches to a prior session by ID.
	ResumeSession(ctx context.Context, id string, spec SessionSpec) (Session, error)

	// CloseSession ends a session and releases runtime resources tied to it.
	// Does not delete persisted transcripts.
	CloseSession(ctx context.Context, id string) error

	// SendTurn queues a new turn on the given session. The returned Turn
	// handle carries the runtime-assigned turn ID; observers stream via
	// Events().
	SendTurn(ctx context.Context, sessionID string, in TurnInput) (Turn, error)

	// InterruptTurn cancels a running turn. Runtimes without native
	// interrupt should return ErrUnsupported.
	InterruptTurn(ctx context.Context, sessionID, turnID string) error

	// SteerTurn injects a mid-turn instruction. Optional; runtimes without
	// steer return ErrUnsupported.
	SteerTurn(ctx context.Context, sessionID, turnID, text string) error

	// Events returns a channel that emits normalized events from any
	// currently open session. Callers do their own multiplexing by
	// SessionID/TurnID on the Event payload.
	Events() <-chan Event

	// ResolveApproval answers a runtime-issued approval request. Codex
	// exposes these as first-class RPC calls; Claude exposes them via the
	// PermissionRequest hook. Both funnel through this method.
	ResolveApproval(ctx context.Context, r ApprovalResponse, d ApprovalDecision) (bool, error)

	// InstallPersona writes the runtime-native persona files under
	// projectDir from the canonical shipmates persona spec.
	InstallPersona(ctx context.Context, projectDir string, p PersonaSpec) error

	// UninstallPersona removes the runtime-native persona files under
	// projectDir for the named persona.
	UninstallPersona(ctx context.Context, projectDir, personaName string) error

	// InstallMemoryHook wires up the runtime's "read memory on session
	// start" mechanism. For Claude that's a SessionStart hook in
	// .claude/settings.json; for Codex it's whatever the equivalent is.
	InstallMemoryHook(ctx context.Context, projectDir string) error

	// Close shuts the runtime down. Idempotent.
	Close(ctx context.Context) error
}

// Caps reports optional runtime capabilities. Zero-value means "no optional
// features supported"; every runtime is expected to at minimum support
// StartSession, SendTurn, and Events.
type Caps struct {
	// Streaming: Events() delivers incremental turn output as it happens.
	// If false, callers should expect events only on turn completion.
	Streaming bool
	// Interrupt: InterruptTurn is honored server-side.
	Interrupt bool
	// Steer: SteerTurn is honored server-side.
	Steer bool
	// Attachments: TurnInput.Attachments are accepted.
	Attachments bool
	// Refusal: runtime distinguishes protocol refusal from other errors.
	Refusal bool
	// Containment: runtime supports SessionSpec.ContainExec (cgroup-based
	// process containment). Only true on Linux with the cgroup launcher
	// available.
	Containment bool
	// Environment: SessionSpec.Environment overrides are applied to the
	// runtime's spawned processes. Runtimes without per-session process
	// environments (codex app-server) report false and reject specs that
	// set Environment with ErrUnsupported.
	Environment bool
	// Approvals: the runtime asks before running tool calls it cannot
	// approve itself, emitting KindApprovalNeeded events that ResolveApproval
	// answers. When false, ResolveApproval returns ErrUnsupported and callers
	// must not present the runtime as mediated.
	Approvals bool
}

// Session is a handle to an active or resumable runtime session.
type Session interface {
	ID() string
	Persona() string
	ProjectDir() string
}

// Turn is a handle to an in-flight turn within a session.
type Turn interface {
	ID() string
	SessionID() string
	// StartedAt is the runtime-side start time.
	StartedAt() time.Time
}

// SessionSpec is what StartSession/ResumeSession need to launch a runtime
// session.
type SessionSpec struct {
	// Persona is the shipmates persona name (matches a directory under
	// catalog/ or .shipmates/personas/).
	Persona string
	// ProjectDir is the project root the session operates within.
	ProjectDir string
	// WorkingDir is where the runtime's shell/tool calls originate. Defaults
	// to ProjectDir if empty.
	WorkingDir string
	// Environment overrides for the runtime process. Honored only when
	// Caps.Environment is true (claude applies it to every spawned turn;
	// codex's app-server transport cannot carry per-session environments
	// and rejects a non-empty value with ErrUnsupported).
	Environment map[string]string
	// ContainExec requests cgroup-based process containment. Ignored (with
	// a warning) if the runtime returns Caps.Containment == false.
	ContainExec bool
}

// TurnInput is a single input to a turn — text plus optional attachments.
type TurnInput struct {
	Text        string
	Attachments []Attachment
}

// Attachment is a file included with a turn, already validated and read by
// the caller (see internal/turninput and internal/attach). Runtimes decide
// how to encode it: codex passes the local path, claude emits a base64
// content block.
//
// Only attachments a runtime can carry natively appear here — today that
// means images. Text is inlined into TurnInput.Text and binary files are
// referenced by path in TurnInput.Text, both by internal/attach, so no
// runtime has to decide what to do with an arbitrary byte stream.
type Attachment struct {
	// Path is the validated absolute path inside the project root.
	Path string
	// DisplayPath is the project-relative, slash-normalized path. Safe to
	// put in a prompt or show an operator.
	DisplayPath string
	// Kind is a hint: "image", "text", "binary".
	Kind string
	// MediaType is the IANA media type, e.g. "image/png". Required by
	// runtimes that emit typed content blocks.
	MediaType string
	// Size is the file size in bytes at validation time.
	Size uint64
	// Base64 is the standard-encoded file content. Populated by the caller
	// immediately before the turn is sent, after revalidating the file.
	Base64 string
}

// Event is the normalized cross-runtime event shape. Payload is per-Kind.
type Event struct {
	Timestamp time.Time
	Kind      Kind
	SessionID string
	TurnID    string
	// Payload holds Kind-specific data. Consumers type-assert.
	Payload any
}

// Kind enumerates the normalized event kinds. Runtimes emit a subset;
// unknown/backend-specific events are folded into KindBackend with the raw
// payload preserved.
type Kind string

const (
	KindText           Kind = "text"
	KindToolCall       Kind = "tool_call"
	KindToolResult     Kind = "tool_result"
	KindApprovalNeeded Kind = "approval_needed"
	KindTurnDone       Kind = "turn_done"
	KindSessionClosed  Kind = "session_closed"
	KindError          Kind = "error"
	KindBackend        Kind = "backend" // runtime-specific escape hatch
)

// ApprovalResponse carries a runtime's approval request through shipmates so
// a human or policy engine can decide. Runtimes populate this from their
// own approval protocol; shipmates presents it uniformly.
type ApprovalResponse struct {
	ID            string
	SessionID     string
	TurnID        string
	Kind          string // "tool_use", "file_write", "network", etc.
	Reason        string
	PolicyContext map[string]any
}

// ApprovalDecision is the human or policy answer.
type ApprovalDecision struct {
	Allow     bool
	Rationale string
	// AllowFor grants persistent scope: "session", "project", "once".
	AllowFor string
}

// PersonaSpec is the canonical shipmates persona shape. Runtimes translate
// this to their native persona file format during InstallPersona.
// A persona's berth is deliberately absent: which working directory a
// session runs in is shipmates' own launch concern, configured per project in
// shipmates.yaml (project.CrewOverride.Berth) and applied through
// SessionSpec.WorkingDir. No runtime can express it, every runtime installer
// elided it, and carrying it here only invited two half-wired spellings of
// one setting. See internal/berth and docs/persona-berths.md.
type PersonaSpec struct {
	Name         string
	Description  string
	Capabilities []string // canonical capability names: "read", "edit", "bash", "browse"
	// Model is optional; empty means the runtime's default.
	Model string
	// SystemPrompt overrides the persona's system prompt. Rarely used.
	SystemPrompt string
	// MemorySeeds are file paths (relative to the persona catalog dir) that
	// should be copied into .shipmates/memory/<persona>/ on install.
	MemorySeeds []string
}

// ErrUnsupported is returned by runtime methods when the requested feature
// isn't in this runtime's Caps.
type ErrUnsupported struct {
	Runtime string
	Feature string
}

func (e *ErrUnsupported) Error() string {
	return "runtime " + e.Runtime + ": " + e.Feature + " not supported"
}

// Factory returns Runtime instances by name. Config parsing lives in a
// separate package; Factory just maps a config-selected name + settings
// into a live Runtime.
type Factory interface {
	New(ctx context.Context, name string, settings map[string]any) (Runtime, error)
	Names() []string
}
