# Runtime interface plan

Owner: @luthermonson · Started: 2026-07-23

## Why this exists

Shipmates was originally a Claude Code driver. The `codex-adaptation` branch
replaced Claude with OpenAI Codex, narrowed to Linux-only, and added
containment + voyage + recovery + Fleet Commander M3. We keep everything the
branch added, but we restore Claude as a first-class option and preserve
multi-platform reach.

Direction: introduce a `Runtime` interface with **Claude Code and Codex as
peer implementations**, selected by config. Cgroup-based containment stays
Linux-only, gated behind `_linux.go` files with stub siblings on other
platforms so the rest of shipmates compiles and runs everywhere.

## The interface

Modeled after `internal/codexapp.Adapter`, which is already close to
runtime-agnostic in shape:

```go
package runtime

type Runtime interface {
    Name() string        // "claude" | "codex"
    Capabilities() Caps

    // Session lifecycle (aka "thread" in Codex parlance)
    StartSession(ctx context.Context, spec SessionSpec) (Session, error)
    ResumeSession(ctx context.Context, id string, spec SessionSpec) (Session, error)
    CloseSession(ctx context.Context, id string) error

    // Turns
    SendTurn(ctx context.Context, sessionID string, in TurnInput) (Turn, error)
    InterruptTurn(ctx context.Context, sessionID, turnID string) error
    SteerTurn(ctx context.Context, sessionID, turnID, text string) error

    // Event stream — normalized across runtimes
    Events() <-chan Event

    // Approvals (both runtimes have this concept; Codex calls them approvals,
    // Claude has PermissionRequest hook responses)
    ResolveApproval(ctx context.Context, r ApprovalResponse, d ApprovalDecision) (bool, error)

    Close(ctx context.Context) error
}

type Caps struct {
    Streaming, Interrupt, Steer, Attachments, Refusal, Containment bool
}

type SessionSpec struct {
    Persona      string
    ProjectDir   string
    WorkingDir   string
    Environment  map[string]string
    ContainExec  bool // request cgroup containment if runtime supports it
}

type TurnInput struct {
    Text        string
    Attachments []Attachment
}

type Event struct {
    Timestamp time.Time
    Kind      string      // "text" | "tool_call" | "tool_result" | "approval_needed" | "done" | "error"
    SessionID string
    TurnID    string
    Payload   any
}
```

The `runtime` package owns the interface, types, and event normalization.
Runtimes live under `runtime/claude/` and `runtime/codex/`.

## Config

Project-level `.shipmates/config.yaml`, user-level `~/.shipmates/config.yaml`
fallback. `--runtime` CLI flag overrides both.

```yaml
runtime: claude   # default; users flip to codex per project

runtimes:
  claude:
    binary: claude
  codex:
    binary: codex
    containment: cgroup   # linux-only; warned + ignored elsewhere
```

## Cgroup gating strategy

- `internal/codexapp/containment_linux.go` — already correctly named. Keep.
- New: `containment_darwin.go`, `containment_windows.go` — stub
  implementations that return `Caps{Containment: false}` and refuse to
  launch when `ContainExec: true` is requested.
- Tool `shipmates-cgroup-launcher` — add `//go:build linux` at top so it
  simply doesn't build on other platforms.
- `internal/delegation/delegation.go` and
  `internal/fleetcommandermailbox/store.go` currently use `golang.org/x/sys/unix`
  directly with no build tags → break Windows build. Fix by splitting each
  into a portable core + `*_unix.go` file-locking helper + `*_windows.go`
  file-locking helper. First cut: `//go:build unix` gate on the whole file
  with a stub for Windows.

## Persona catalog

Personas stay in the shipmates catalog in a runtime-agnostic canonical form:

```
catalog/
├── captain/
│   ├── persona.md        # canonical shipmates format
│   └── memory-seeds/
```

`InstallPersona` translates to the runtime's native format on install:

- Claude: writes `.claude/agents/<name>.md`
- Codex: writes `.codex/agents/<name>.md` (or wherever Codex reads personas)

## Migration order

Each phase ships independently.

- [x] **Phase 0 — this commit**: design doc + skeleton `internal/runtime`
      package with the interface. Two Linux-only files (`delegation`,
      `fleetcommandermailbox/store`) got `//go:build unix` build tags so
      their intent is documented, but the full cross-platform build fix
      cascaded deeper than expected and is deferred to Phase 5.
- [ ] **Phase 1**: refactor `codexapp.Adapter` to implement `runtime.Runtime`.
      No behavior change on Linux; commands still work.
- [ ] **Phase 2**: restore Claude Code integration as `runtime/claude`
      implementing the same interface. Recover the removed code from
      `origin/main` history and adapt it.
- [ ] **Phase 3**: config loader + `--runtime` flag + factory that returns
      the right implementation.
- [ ] **Phase 4**: introduce canonical persona format + per-runtime
      translators.
- [ ] **Phase 5**: cgroup containment stubs for darwin/windows so the whole
      binary compiles cleanly on all three OSes.

## Non-goals

- Perfect API parity between Codex and Claude. Some capabilities (Steer,
  Refusal) may return `not supported` on Claude. That's fine.
- Runtime auto-switching mid-session. Config-time selection is enough.
- Cross-runtime session migration. Sessions are runtime-scoped.
