// Package factory maps a config-resolved runtime name onto a live
// runtime.Runtime instance. Shipmates commands call NewFromResolved; the
// factory hides which concrete implementation gets constructed AND wires
// the containment mode selected by config into the runtime.
//
// Two runtimes are wired here:
//
//   - claude: constructed directly from Config + a Containment Watcher.
//   - codex: constructed via NewCodexWith with explicit StartOptions,
//     because the app-server transport needs project working directory
//     and containment posture the config file cannot reasonably carry.
//     NewFromResolved surfaces an error pointing at NewCodexWith when
//     codex is selected via config.
package factory

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
	"github.com/luthermonson/shipmates/internal/runtime/codex"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
	"github.com/luthermonson/shipmates/internal/runtime/containment/none"
	"github.com/luthermonson/shipmates/internal/runtime/containment/watchdog"
	"github.com/luthermonson/shipmates/internal/runtime/persona"
)

// NewFromResolved constructs a Runtime from a fully resolved config —
// runtime name + settings + containment mode. Shipmates command code
// should call this (not NewFromConfig) so containment is honored.
//
// TODO(cgroup-watcher): route Codex spawns through containment.Watcher too.
// Codex currently self-contains inside codexapp.Adapter via
// RequireExecutionContainment, so we don't double-wrap here.
func NewFromResolved(ctx context.Context, r config.Resolved) (runtime.Runtime, error) {
	watcher, limits, err := containmentFor(r.Containment)
	if err != nil {
		return nil, err
	}
	switch r.Runtime {
	case "claude":
		cfg := claude.Config{Containment: watcher, Limits: limits}
		if v, ok := r.Settings["binary"].(string); ok {
			cfg.Binary = v
		}
		if raw, ok := r.Settings["default_args"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					cfg.DefaultArgs = append(cfg.DefaultArgs, s)
				}
			}
		}
		return claude.New(cfg), nil

	case "codex":
		// Codex needs codexapp.StartOptions (working dir, containment
		// posture, credential isolation) that the config file cannot
		// reasonably carry. Route callers to NewCodexWith.
		return nil, fmt.Errorf("factory: codex must be constructed via NewCodexWith; NewFromResolved cannot start the app-server transport")

	default:
		return nil, fmt.Errorf("factory: unknown runtime %q", r.Runtime)
	}
}

// NewFromConfig is the backwards-compatible entry point that doesn't take
// a Containment block. Every call goes through the default "watchdog"
// mode with zero limits.
func NewFromConfig(ctx context.Context, name string, settings map[string]any) (runtime.Runtime, error) {
	return NewFromResolved(ctx, config.Resolved{
		Runtime:     name,
		Settings:    settings,
		Containment: config.Containment{Mode: "watchdog"},
	})
}

// NewCodexWith constructs a codex runtime with explicit transport options.
// Prefer this over NewFromConfig / NewFromResolved for codex.
func NewCodexWith(ctx context.Context, opts codexapp.StartOptions) (runtime.Runtime, error) {
	return codex.New(ctx, opts)
}

// containmentFor translates the config-side Containment block into a
// runtime-side Watcher + Limits. "cgroup" is accepted but currently
// degrades to watchdog (real cgroup implementation lands in a follow-up),
// so operators who picked cgroup optimistically still get bounded
// processes.
func containmentFor(c config.Containment) (containment.Watcher, containment.Limits, error) {
	limits := containment.Limits{
		MaxRSSBytes:     c.MemoryLimitMB * 1024 * 1024,
		MaxCPUSeconds:   float64(c.CPULimitSeconds),
		PollInterval:    time.Duration(c.PollIntervalMS) * time.Millisecond,
		GracefulTimeout: time.Duration(c.GracefulTimeoutMS) * time.Millisecond,
	}
	if c.MaxProcesses > 0 {
		limits.MaxProcesses = uint32(min(c.MaxProcesses, int64(^uint32(0))))
	}
	switch c.Mode {
	case "", "watchdog":
		return watchdog.New(), limits, nil
	case "none":
		return none.New(), containment.Limits{}, nil
	case "cgroup":
		// TODO: land the cgroup Watcher adapter as a separate commit. For
		// now degrade gracefully rather than fail hard — but say so, since
		// the operator asked for kernel-enforced containment and is getting
		// the polling watchdog instead.
		slog.Warn("containment mode cgroup requested; cgroup adapter not yet implemented, degrading to watchdog")
		return watchdog.New(), limits, nil
	default:
		return nil, containment.Limits{}, fmt.Errorf("factory: unknown containment mode %q", c.Mode)
	}
}

// Names returns the runtime names this factory knows about.
func Names() []string { return []string{"claude", "codex"} }

// InstallMemoryHook wires the named runtime's "read memory on session
// start" mechanism into projectDir.
//
// It exists separately from NewFromResolved because installing the hook is
// a file operation: `shipmates init`, `add` and `update` must be able to do
// it without starting a runtime transport (codex would need an app-server;
// claude would need the CLI on PATH). Dispatching by name here is also what
// keeps a project free of the other runtime's configuration — a codex-only
// project never grows a .claude/ directory, and vice versa.
//
// An unknown runtime name is not an error: an operator who has selected a
// runtime shipmates does not know about should not have `init` fail over a
// hook.
func InstallMemoryHook(name, projectDir string) error {
	switch name {
	case "claude":
		return claude.InstallMemoryHookAt(projectDir)
	case "codex":
		return codex.InstallMemoryHookAt(projectDir)
	default:
		slog.Debug("no memory-hook mechanism for runtime", "runtime", name)
		return nil
	}
}

// PersonaArtifactPath returns the project-relative path of the named
// runtime's own persona artifact, and whether it has one at all.
//
// Only claude does today. Codex's artifact — `.codex/agents/<persona>.toml`
// — is deliberately absent here: it is shipmates' canonical persona
// inventory (`list`, `remove` and `update` read it on every runtime), so it
// is written unconditionally by `add`/`update` and is not a per-runtime
// artifact this seam may skip.
//
// The path is what `add`/`update` record in the install manifest, so an
// artifact is tracked, preserved when hand-edited, and removed with the
// persona.
func PersonaArtifactPath(name, personaName string) (string, bool) {
	if name == "claude" {
		return claude.AgentPath(personaName), true
	}
	return "", false
}

// PersonaArtifact renders a canonical persona into the named runtime's own
// artifact: the project-relative path and its exact bytes.
//
// It exists separately from Runtime.InstallPersona for the same reason
// InstallMemoryHook does — installing a persona is a file operation that
// `add`/`update` must perform without starting a runtime transport — and for
// one more: shipmates must decide whether to write at all. Handing back the
// bytes is what lets the caller run them through the install manifest and
// leave an operator's edits alone. Runtime.InstallPersona renders through
// the same function, so both produce identical artifacts.
func PersonaArtifact(name string, c *persona.Canonical) (string, []byte, bool, error) {
	path, ok := PersonaArtifactPath(name, c.Name)
	if !ok {
		slog.Debug("runtime installs no persona artifact of its own", "runtime", name)
		return "", nil, false, nil
	}
	content, err := claude.RenderPersona(c.Spec())
	if err != nil {
		return "", nil, false, err
	}
	return path, content, true, nil
}
