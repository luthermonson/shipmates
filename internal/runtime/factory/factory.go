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
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
	"github.com/luthermonson/shipmates/internal/runtime/codex"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
	"github.com/luthermonson/shipmates/internal/runtime/containment/none"
	"github.com/luthermonson/shipmates/internal/runtime/containment/watchdog"
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
	switch c.Mode {
	case "", "watchdog":
		return watchdog.New(), limits, nil
	case "none":
		return none.New(), containment.Limits{}, nil
	case "cgroup":
		// TODO: land the cgroup Watcher adapter as a separate commit. For
		// now degrade gracefully rather than fail hard.
		return watchdog.New(), limits, nil
	default:
		return nil, containment.Limits{}, fmt.Errorf("factory: unknown containment mode %q", c.Mode)
	}
}

// Names returns the runtime names this factory knows about.
func Names() []string { return []string{"claude", "codex"} }
