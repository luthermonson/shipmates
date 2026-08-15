# The runtime interface

Shipmates used to be Claude Code. Not "built on" it — the same thing. Every
spawn site ran `claude`, every persona was a `.claude/agents/*.md` subagent
file, memory arrived through a Claude Code `SessionStart` hook, and the output
parser decoded Claude's `stream-json`.

That is still the default, and deliberately so. What changed is that it is now
*a* choice rather than *the* implementation: a persona can be backed by Claude
Code, by Codex, or by any OpenAI-compatible endpoint, and the rest of shipmates
talks to whichever one through a single interface.

```
internal/runtime              the interface: Runtime, Caps, events, Selector
internal/runtime/claude       the `claude` CLI over stdio streaming
internal/runtime/codex        the codex app-server transport
internal/runtime/openai       any OpenAI-compatible chat-completions endpoint
internal/runtime/config       which runtime is selected — and the trust boundary
internal/runtime/containment  bounding and tearing down spawned processes
internal/runtime/factory      resolved config + directories -> a live Runtime
internal/runtime/env          the production Selector that commands call
```

## What the interface guarantees

`runtime.Runtime` is a session-based, tool-using agent. Sessions are created or
resumed, turns are sent into them, and normalized events come back on one
channel per runtime which callers multiplex by session and turn.

The part worth being careful about is `Caps`.

Shipmates *branches* on capabilities. It offers steering when `Steer` is true,
presents a runtime as mediating tool calls when `Approvals` is true, and reports
an environment override as applied when `Environment` is true. A runtime that
overstates any of those makes shipmates lie to an operator on its behalf — and
a caller cannot check for itself, which is what makes an inaccurate report so
much worse than a modest one.

So the contract runs both ways:

- A capability reported **false** must make the corresponding method return
  `*runtime.ErrUnsupported`, and must do so *before* validating arguments or
  touching anything. That makes the report cheap to probe and a mistaken call
  harmless.
- A capability reported **true** must not return `ErrUnsupported` for that
  feature.

`Caps`' zero value claims nothing, so a capability is opted into rather than
disclaimed — a new implementation is honest by default and becomes dishonest
only by explicitly saying something untrue.

`runtime.VerifyCaps` checks the first half mechanically. Every runtime should
call it from its tests:

```go
if errs := runtime.VerifyCaps(ctx, rt, t.TempDir()); len(errs) > 0 {
    for _, err := range errs {
        t.Error(err)
    }
}
```

It cannot check the second half. Proving `Steer` is honored takes a live
session, a real turn, and a model that responds to steering; that belongs in a
runtime's own integration tests.

`ErrUnsupported` carries both the runtime and the feature, matches the
`runtime.ErrFeatureUnsupported` sentinel through `errors.Is`, and yields its
fields through `errors.As`. Construct it with `runtime.Unsupported(name,
feature)`.

### One thing the interface does not yet normalize

`Event.Payload` is `any`, and each runtime owns its own payload vocabulary. For
`KindText` that means three different shapes today: claude sends a bare
`string`, openai sends an `openai.TextDelta`, codex sends its backend event.
A consumer that wants to render text from an arbitrary runtime therefore has to
import all three, or type-switch on shapes it cannot enumerate.

That is fine while every renderer is runtime-specific, and it is the first
thing to fix when one is not. A shared text payload type — or a
`TextContent() string` interface that payloads may satisfy — belongs in
`internal/runtime` at that point.

## Selecting a runtime

Precedence, highest first:

1. `--runtime` on the command line (or `SHIPMATES_RUNTIME`)
2. `runtime:` in the project's `.shipmates/config.yaml`
3. `runtime:` in the operator's `~/.shipmates/config.yaml`
4. the default: **claude**

The default is claude because that is what shipmates has always launched.
Making the runtime pluggable must not move anybody off it. An operator opts in
to something else; nobody gets migrated by an upgrade.

An unrecognized name is an error at every layer, from whichever layer supplied
it. Shipmates will not accept a runtime name it cannot then build.

### The trust boundary

**A project's `.shipmates/config.yaml` may select which runtime a project uses.
It may not influence what that runtime executes, with what arguments, or under
what containment.**

This is a security property, not a preference. A project config file arrives
with the checkout. On a cloned repository it is attacker-controlled content that
shipmates reads before the operator has reviewed anything. If it could set a
binary path and an argument vector, then `git clone` plus any shipmates command
would be arbitrary code execution — with the operator's credentials, in the
operator's shell. If it could set `containment: {mode: none}`, a repository
could strip the resource bounds off every turn it ran.

The two files are therefore **different schemas**:

```yaml
# .shipmates/config.yaml — project. This is the entire schema.
runtime: codex
```

```yaml
# ~/.shipmates/config.yaml — operator. The full schema.
runtime: claude

runtimes:
  claude:
    binary: /opt/homebrew/bin/claude      # WHAT gets executed
    default_args: ["--model", "opus"]     # and with which arguments
  codex:
    command: ["/opt/codex/bin/codex", "app-server", "--stdio"]
    startup_timeout_ms: 20000
  openai:
    base_url: https://inference.example.com/v1
    model: qwen3-coder
    api_key_env: INFERENCE_TOKEN          # the NAME of the env var, never the key

containment:
  mode: watchdog          # none | watchdog
  memory_limit_mb: 8192   # 0 = uncapped
  cpu_limit_seconds: 0
  max_processes: 0        # kernel-enforced on Windows only
  poll_interval_ms: 500
  graceful_timeout_ms: 2000
```

The enforcement is a type, not a filter. `config.ProjectFile` has one field for
the runtime name and no field for anything else, so there is nothing for a
project file's `runtimes:` or `containment:` block to decode into. A filter
would be a denylist, and the first execution-shaped key someone adds without
remembering to update the denylist reopens the hole. A type cannot forget —
and `TestProjectFile_TrustedFieldsOnly` fails if a field is added to it.

Keys a project file sets and is not trusted to supply are reported rather than
silently dropped: `LoadProject` records them, `env.Selector.Resolve` warns
once, and the list travels on the resolution so a command can surface it.

## Containment

Containment posture is the operator's decision, applied to the processes a
runtime spawns.

| mode | what it does |
| --- | --- |
| `watchdog` | **default.** Native process-tree teardown — Unix process groups, Windows Job Objects — plus a sampler that reads RSS and CPU time every `poll_interval_ms` and kills the tree on breach. Zero privileges, no install step. On Windows the memory and process-count caps are additionally programmed on the Job Object, so the kernel enforces them with no polling gap. |
| `none` | No bounds. An explicit escape hatch, and a real implementation rather than a nil watcher, so "containment off" is a choice in config instead of a special case in every caller. |

Every process-spawning runtime goes through the same watcher. `claude` binds it
as its `Supervisor`; `codex` binds it into `codexapp.StartOptions.Supervisor`
through `codex.Contain`, so the app-server child and everything it spawns are
inside one process group (or one Job Object) with the operator's limits on it.
`openai` spawns nothing and reports `Caps.Containment: false`.

The default imposes **no resource caps**. `watchdog` with an empty budget buys
process-tree teardown and nothing else, which is the right default: it fixes
orphaned children without inventing a memory limit nobody asked for.

`Limits` treats a zero field as unlimited, and a watchdog with no limits starts
no sampler at all rather than polling for a breach that cannot happen.

### The removed `cgroup` mode

`mode: cgroup` used to select a Linux-only, kernel-enforced containment path
that lived inside `internal/codexapp`, was reachable from no other runtime, and
whose only real test was gated behind an environment variable and had therefore
never run in CI. It has been **removed**, implementation and mode alike.

A config that still names it now **fails to resolve**, with an error pointing at
`watchdog`. It is deliberately not accepted-and-degraded: an operator who chose
kernel enforcement and silently received polling enforcement has been told
something untrue about their own deployment. Limits carry over unchanged —
`memory_limit_mb`, `cpu_limit_seconds` and `max_processes` all mean the same
thing under `watchdog`, and on Windows the memory and process-count caps are
still kernel-enforced through the Job Object.

## How `--runtime` is honored

Every command whose behavior depends on the runtime offers `--runtime`, and no
command offers it without honoring it. `TestRuntimeFlag_OfferedByEveryCommandThatHonorsIt`
asserts both halves against the real command tree, because "accepted by two
commands and silently ignored by the rest" is how this went wrong before.

| commands | why they consult the runtime |
| --- | --- |
| `open`, `ask`, `fanout`, `drain`, `drain-many`, `server serve`, `sail` | they launch a session |
| `init`, `add`, `update`, `remove`, `routing apply` | they install or reconcile the runtime's persona artifacts |

Commands that talk to the coordination server, read local state, or render for
other tools (`tell`, `feed`, `pending`, `list`, `render`, …) do not take the
flag, because there would be nothing for them to do with it.

The flag is per-command rather than persistent on the root command, so it goes
*after* the subcommand:

```
shipmates ask --runtime codex captain "status?"     # yes
shipmates --runtime codex ask captain "status?"     # no: unknown flag at root
```

That costs a little ergonomics and buys the property above — a command that does
not honor a runtime selection does not accept one either. `SHIPMATES_RUNTIME`
works from anywhere and is reported as itself, not as a flag nobody typed.

### What honoring means today, honestly

Selection is fully pluggable. The **launch path is not, yet.**

The commands that spawn a session still exec the `claude` CLI directly with
Claude Code's own session flags (`--resume` / `--session-id` / `--agent`, from
`internal/project.SessionLaunch`), and `internal/server` drives it over
Claude's `stream-json` protocol and a PTY. Persona artifacts are likewise
Claude's `.claude/agents/*.md`.

So with a non-Claude runtime selected, those commands **refuse, and say why** —
naming the runtime, where the selection came from, and what is missing. The two
alternatives were both worse: running claude anyway ignores an explicit
instruction, and accepting the flag while doing nothing with it is the exact
failure this wiring exists to prevent.

`init` is already fully dispatched: it installs the memory mechanism of the
runtime it resolved, through `factory.InstallMemoryHook`. Claude gets a
`SessionStart` hook in `.claude/settings.json`; a runtime that folds memory into
the prompt itself gets nothing written. A codex-only project never grows a
`.claude/` directory.

### Finishing the job

What the remaining step needs:

1. **A generic turn renderer.** `internal/commands.dispatchTo` builds Claude's
   argv and streams Claude's stdout. Its replacement sends a `TurnInput` and
   drains `Events()` — which needs the shared text payload noted above.
2. **Runtime-agnostic session bookkeeping.** `project.SessionLaunch` emits
   Claude's session flags and `project.WriteSessionMeta` records a Claude
   session id and fingerprint. Both need to move behind
   `Runtime.StartSession` / `ResumeSession`.
3. **A persona artifact seam for every runtime.** `factory.PersonaArtifact`
   returns path-plus-bytes so `add` and `update` can hash an artifact, compare
   it against the install manifest, and leave an operator's hand-edits alone —
   without starting a transport to do it. Only claude exposes the path/render
   pair that needs. Codex and openai both *have* persona formats and both write
   them from `Runtime.InstallPersona` (`.codex/agents/<persona>.md` and
   `.shipmates/runtimes/openai/personas/<persona>.md`); what is missing is the
   reconciliation seam, which is why `add` and `update` refuse them rather than
   the runtimes being incapable.
4. **Per-runtime manifest namespacing.** `project.Manifest.Files` is a flat
   `path -> sha` map with no runtime attribution, so two runtimes' artifacts
   would share one namespace.
5. **The server.** `internal/server` is the deepest Claude coupling: PTY
   handling, bracketed-paste injection, `stream-json` decoding, and a
   stderr-scraped stale-session probe.

None of that changes the selection layer, which is why it ships first.
