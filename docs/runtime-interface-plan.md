# Runtime interface plan

Owner: @luthermonson · Started: 2026-07-23 · Status: Phase 3 landed,
Phase 6 started, in [PR #14](https://github.com/luthermonson/shipmates/pull/14)
— the `runtime` interface, both `claude` and `codex` adapters, the
containment watcher, the config loader (`.shipmates/config.yaml`), the
factory, and the global `--runtime` CLI flag are on
`feat/runtime-interface`, and `ask` is the first command dispatching
through the Selector. The rest of the command surface migration
(Phase 4+) is not yet done.

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
runtime: claude   # built-in default is codex; set claude per project to opt in

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

- [x] **Phase 0**: design doc + skeleton `internal/runtime` package with
      the interface. Two Linux-only files (`delegation`,
      `fleetcommandermailbox/store`) got `//go:build unix` build tags so
      their intent is documented; the full cross-platform build fix
      cascaded deeper than expected and landed in Phase 5.
- [x] **Phase 1**: `codexapp.Adapter` now implements `runtime.Runtime`
      via `internal/runtime/codex`. Codex-native command dispatch is
      unchanged.
- [x] **Phase 2**: Claude Code integration restored as
      `internal/runtime/claude`, with stdio streaming, event
      normalization, persona install (`.claude/agents/<name>.md`), and
      the `SessionStart` memory hook.
- [x] **Phase 3**: config loader (`.shipmates/config.yaml` +
      `~/.shipmates/config.yaml`), the global `--runtime` CLI flag /
      `SHIPMATES_RUNTIME` env var, and the `internal/runtime/factory`
      that returns the right implementation. `env.Selector` is the
      command-side entry point; codex returns `ErrNotConfigured` from
      the selector because it needs `codexapp.StartOptions` that the
      config file cannot reasonably carry.
- [ ] **Phase 4**: introduce canonical persona format (`persona.md`) +
      per-runtime translators, and migrate `shipmates init` / `add` /
      `update` / `render` onto the runtime interface so `init` emits
      both `.codex/agents/*.toml` and `.claude/agents/*.md` per the
      selected runtime.
- [ ] **Phase 5**: cgroup containment stubs for darwin/windows so the
      whole binary compiles cleanly on all three OSes. (Cross-platform
      build achieved by the release workflow via `//go:build unix`
      gating of Sail + Fleet Commander M1-M3 instead — see
      [`docs/platform-support.md`](platform-support.md).)
- [~] **Phase 6 (in progress)**: migrate the dispatch commands onto
      `env.Selector` / `factory.NewFromResolved`.
      - `ask` landed first: it resolves the runtime via the Selector,
        dispatches through the runtime interface when the selection is
        claude (with session persistence in
        `.shipmates/sessions/<persona>.claude.session`), and falls back
        to the codex-native dispatcher when the selection is codex (the
        default).
      - `show` (restored, all-file) uses the same routing, and adds
        attachment delivery: images natively, text inlined and bounded,
        binary referenced by path.
      - **The live-session surface landed**: `internal/livesession` now
        consumes `runtime.Runtime` through a narrow `Backend` seam that
        `*codexapp.Adapter` satisfies structurally (so the codex path is
        unchanged) and a `runtimeBackend` adapter satisfies for any
        `runtime.Runtime`. `live`, `tell`, `feed`, `interrupt`, and
        `show`-into-a-running-turn therefore work on claude. Codex is
        thread+turn and claude is session+turn; both map onto the
        existing (session, thread, turn) tuple with the runtime session
        id in the thread slot, so exact-turn targeting is preserved
        verbatim. Continuity markers are per-backend.
      - Known gaps on the claude live path: `ResolveApproval` returns
        `ErrUnsupported` (approval mediation is Phase 4 hook plumbing),
        and no claude event maps to `KindApprovalNeeded` yet, so an
        approval-needing tool call cannot be mediated there.
      - Remaining: `open` (the terminal dashboard) still assumes the
        codex-native controller surface, and the queue workflows
        (`fanout`, `drain`, `drain-many`, `autonomous`) plus `sail` and
        `plan` still call the codex-native dispatcher directly.
- [ ] **Phase 7**: migrate Sail + Fleet onto the interface, once the
      unix-only dependencies (PID-file dispatch locks, `openat`,
      `flock`, `/proc`) are abstracted; see
      [Platform support](platform-support.md).

## Non-goals

- Perfect API parity between Codex and Claude. Some capabilities (e.g.
  Refusal, ResolveApproval) return `not supported` on Claude. That's
  fine — the gap is surfaced as a runtime-scoped error, never faked or
  silently no-opped. (Steer, Interrupt, and Attachments landed on Claude
  via the persistent stream-json transport.) Symmetrically, shipmates
  sends codex mid-turn steer input as text only, so a `show` into a
  running codex turn references images by path instead of attaching
  them.
- Runtime auto-switching mid-session. Config-time selection is enough.
- Cross-runtime session migration. Sessions are runtime-scoped.
