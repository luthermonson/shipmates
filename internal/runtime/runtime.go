// Package runtime is the shipmates AI-runtime abstraction. Implementations
// live under runtime/claude, runtime/codex and runtime/openai; the rest of
// shipmates depends only on this interface. See docs/runtime-interface.md for
// the design, the config schema and the trust boundary.
package runtime

import (
	"context"
	"errors"
	"time"
)

// Runtime is a session-based, tool-using coding agent: Claude Code, Codex, an
// OpenAI-compatible endpoint, or a future addition.
//
// # Capabilities and honesty
//
// Not every runtime can do everything, and the interface does not pretend
// otherwise. [Caps] declares what an implementation supports, and callers
// branch on it. The contract in both directions:
//
//   - A capability reported false MUST make the corresponding method return
//     an [*ErrUnsupported], and it must do so before validating arguments or
//     touching anything, so a caller can probe cheaply and so a mistaken call
//     has no side effects.
//   - A capability reported true MUST NOT return [*ErrUnsupported] for that
//     feature.
//
// Reporting a capability you do not have is worse than reporting none: a
// runtime that claims Approvals and then discards them tells shipmates it is
// mediating tool calls when it is not. [VerifyCaps] checks the first half of
// this contract mechanically; implementations are expected to run it.
type Runtime interface {
	// Name identifies the runtime for configuration and logging: "claude",
	// "codex", "openai". It must match the name the runtime is registered
	// under, and the Runtime field of any ErrUnsupported it returns.
	Name() string

	// Capabilities reports what this runtime can do. Callers must check
	// before requesting an optional feature.
	Capabilities() Caps

	// StartSession begins a fresh persona session (Codex calls this a
	// "thread"). Returns a handle to the session.
	StartSession(ctx context.Context, spec SessionSpec) (Session, error)

	// ResumeSession attaches to a prior session by ID.
	ResumeSession(ctx context.Context, id string, spec SessionSpec) (Session, error)

	// CloseSession ends a session and releases the runtime resources tied to
	// it. It does not delete persisted transcripts.
	CloseSession(ctx context.Context, id string) error

	// SendTurn queues a new turn on the given session. The returned Turn
	// carries the runtime-assigned turn ID; observers stream via Events.
	SendTurn(ctx context.Context, sessionID string, in TurnInput) (Turn, error)

	// InterruptTurn cancels a running turn. Requires Caps.Interrupt.
	InterruptTurn(ctx context.Context, sessionID, turnID string) error

	// SteerTurn injects a mid-turn instruction. Requires Caps.Steer.
	SteerTurn(ctx context.Context, sessionID, turnID, text string) error

	// Events returns a channel that emits normalized events from every
	// currently open session. It is a single channel per Runtime, not per
	// session: callers multiplex on Event.SessionID and Event.TurnID. The
	// channel is closed by Close.
	Events() <-chan Event

	// ResolveApproval answers a runtime-issued approval request. Codex
	// exposes these as first-class RPC calls; Claude exposes them through the
	// PermissionRequest hook. Both funnel through here. The bool reports
	// whether the decision was delivered to a request that was still
	// waiting. Requires Caps.Approvals.
	ResolveApproval(ctx context.Context, r ApprovalResponse, d ApprovalDecision) (bool, error)

	// InstallPersona writes the runtime-native persona files under projectDir
	// from the canonical shipmates persona spec. Requires
	// Caps.PersonaInstall.
	//
	// Callers that need the artifact WITHOUT starting a transport — `add` and
	// `update` reconcile it against the install manifest, and neither should
	// have to launch a CLI or an app-server to do so — go through
	// factory.PersonaArtifact instead, which renders through the same code.
	InstallPersona(ctx context.Context, projectDir string, p PersonaSpec) error

	// UninstallPersona removes the runtime-native persona files under
	// projectDir for the named persona. Requires Caps.PersonaInstall.
	// Removing an artifact that was never written is not an error.
	UninstallPersona(ctx context.Context, projectDir, personaName string) error

	// InstallMemoryHook wires up the runtime's "read memory on session start"
	// mechanism. For Claude that is a SessionStart hook in
	// .claude/settings.json. Requires Caps.MemoryHook — and see the note
	// there on why ErrUnsupported means "nothing needed" rather than "memory
	// will not be read".
	InstallMemoryHook(ctx context.Context, projectDir string) error

	// Close shuts the runtime down and closes the Events channel. Idempotent.
	Close(ctx context.Context) error
}

// Caps reports what a runtime supports. The zero value means "nothing
// optional", so a capability is something an implementation opts into rather
// than something it forgets to disclaim.
//
// Every runtime is expected to support at least StartSession, SendTurn,
// Events and Close; those are not capabilities because a type that cannot do
// them is not a Runtime.
type Caps struct {
	// Streaming: Events delivers incremental turn output as it happens. When
	// false, callers should expect events only at turn completion.
	Streaming bool
	// Interrupt: InterruptTurn is honored.
	Interrupt bool
	// Steer: SteerTurn is honored.
	Steer bool
	// Attachments: TurnInput.Attachments are delivered to the model. A
	// runtime that reports false must reject a turn carrying attachments
	// rather than drop them silently — a caption the model never saw is a
	// worse answer than an error.
	Attachments bool
	// Refusal: the runtime distinguishes a model refusal from other errors,
	// so a KindError payload can be classified.
	Refusal bool
	// Containment: spawned processes are routed through the
	// containment.Watcher the runtime was constructed with, so operator
	// limits and process-tree teardown apply. A runtime that talks to a
	// remote endpoint and spawns nothing reports false.
	Containment bool
	// Environment: SessionSpec.Environment is applied to the processes the
	// runtime spawns. A runtime with no per-session process environment
	// reports false and rejects a spec that sets Environment with
	// ErrUnsupported, rather than starting a session under the wrong
	// environment.
	Environment bool
	// Approvals: the runtime asks before running tool calls it cannot
	// approve itself, emitting KindApprovalNeeded events that
	// ResolveApproval answers. When false, ResolveApproval returns
	// ErrUnsupported and callers must not present the runtime as mediated.
	Approvals bool
	// PersonaInstall: the runtime has a persona format of its own that
	// InstallPersona writes and the runtime reads back. False means the
	// runtime needs no artifact, and InstallPersona and UninstallPersona
	// return ErrUnsupported.
	PersonaInstall bool
	// MemoryHook: the runtime has a session-start hook mechanism, so
	// InstallMemoryHook writes something.
	//
	// False does NOT mean a persona's memory goes unread — a runtime may
	// assemble it into the prompt itself on every session, which needs no
	// hook and no file on disk. It means only that there is nothing to
	// install, and InstallMemoryHook returns ErrUnsupported. Callers wiring
	// up a project must therefore treat ErrUnsupported here as "nothing
	// needed", not as a failure.
	MemoryHook bool
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

// SessionSpec is what StartSession and ResumeSession need to launch a session.
//
// Note what it does NOT carry: the containment budget. Which limits apply is
// the operator's decision, resolved once from user config and baked into the
// runtime at construction (see internal/runtime/config and
// internal/runtime/containment). A per-session limits field would be one more
// place for a caller — or a project checkout that reached a caller — to ask
// for less containment than the operator chose. ContainExec below is only a
// request for the kernel-enforced flavor, never a way to ask for less.
type SessionSpec struct {
	// Persona is the shipmates persona name (matching a directory under
	// catalog/ or .shipmates/personas/).
	Persona string
	// ProjectDir is the project root the session operates within.
	ProjectDir string
	// WorkingDir is where the runtime's shell and tool calls originate.
	// Defaults to ProjectDir when empty. This is where a persona's berth
	// lands — see internal/berth.
	WorkingDir string
	// Environment overrides for the runtime's spawned processes. Honored
	// only when Caps.Environment is true; a runtime that reports false must
	// reject a non-empty value with ErrUnsupported.
	Environment map[string]string
	// ContainExec requests kernel-enforced process containment (cgroup
	// delegation) for this session's execution, over and above the watcher
	// the runtime was constructed with. Honored only when Caps.Containment
	// is true.
	//
	// Unlike the other optional features, a runtime that cannot honor it
	// IGNORES it rather than failing: the operator's baseline containment
	// still applies, so the session is not less bounded than it would have
	// been, and refusing to start a session over an unavailable upgrade
	// would be the wrong trade. A runtime that ignores it should say so
	// through an event rather than in silence.
	ContainExec bool
}

// TurnInput is a single input to a turn — text plus optional attachments.
type TurnInput struct {
	Text        string
	Attachments []Attachment
}

// Attachment is a file included with a turn, already validated and read by
// the caller. Runtimes decide how to encode it: one may pass the local path,
// another emits a base64 content block.
//
// Only attachments a runtime can carry natively appear here — today that
// means images. Text is inlined into TurnInput.Text and binary files are
// referenced by path in TurnInput.Text, both by the caller, so no runtime has
// to decide what to do with an arbitrary byte stream.
type Attachment struct {
	// Path is the validated absolute path inside the project root.
	Path string
	// DisplayPath is the project-relative, slash-normalized path. Safe to put
	// in a prompt or show an operator.
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
// backend-specific events are folded into KindBackend with the raw payload
// preserved rather than dropped.
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

// ApprovalResponse carries a runtime's approval request through shipmates so a
// human or policy engine can decide. Runtimes populate it from their own
// approval protocol; shipmates presents it uniformly.
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
	// AllowFor grants persistent scope: "once", "session", "project".
	AllowFor string
}

// PersonaSpec is the canonical shipmates persona shape. Runtimes translate it
// into their native persona format during InstallPersona.
//
// A persona's berth is deliberately absent: which working directory a session
// runs in is shipmates' own launch concern, configured per project in
// shipmates.yaml (project.CrewOverride.Berth) and applied through
// SessionSpec.WorkingDir. No runtime can express it, every runtime installer
// elided it, and carrying it here only invited two half-wired spellings of one
// setting. See internal/berth and docs/persona-berths.md.
type PersonaSpec struct {
	Name         string
	Description  string
	Capabilities []string // canonical capability names: "read", "edit", "bash", "browse"
	// Model is optional; empty means the runtime's default.
	Model string
	// SystemPrompt overrides the persona's system prompt. Rarely used.
	SystemPrompt string
	// MemorySeeds are file paths (relative to the persona catalog dir) to be
	// copied into .shipmates/memory/<persona>/ on install.
	MemorySeeds []string
}

// ErrFeatureUnsupported is the sentinel every [*ErrUnsupported] matches, so a
// caller can ask the general question without knowing the concrete type:
//
//	if errors.Is(err, runtime.ErrFeatureUnsupported) { /* degrade */ }
//
// Use [Unsupported] to construct one.
var ErrFeatureUnsupported = errors.New("runtime feature not supported")

// ErrUnsupported is returned by a runtime method whose capability is not in
// that runtime's [Caps]. It names both the runtime and the feature, because
// "not supported" without either is unactionable in a log.
type ErrUnsupported struct {
	// Runtime is the runtime's Name().
	Runtime string
	// Feature is the method or capability, e.g. "SteerTurn".
	Feature string
}

// Unsupported builds the error a runtime returns for a capability it lacks.
func Unsupported(runtimeName, feature string) *ErrUnsupported {
	return &ErrUnsupported{Runtime: runtimeName, Feature: feature}
}

func (e *ErrUnsupported) Error() string {
	return "runtime " + e.Runtime + ": " + e.Feature + " not supported"
}

// Is makes every ErrUnsupported match ErrFeatureUnsupported, so callers can
// test the category with errors.Is and the specifics with errors.As.
func (e *ErrUnsupported) Is(target error) bool { return target == ErrFeatureUnsupported }

// Factory returns Runtime instances by name. Config parsing lives elsewhere;
// a Factory just maps a resolved name plus its settings onto a live Runtime.
// internal/runtime/factory implements this over its registry.
type Factory interface {
	New(ctx context.Context, name string, settings map[string]any) (Runtime, error)
	Names() []string
}
