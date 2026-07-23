// Package factory maps a config-resolved runtime name onto a live
// runtime.Runtime instance. Shipmates commands call NewFromConfig; the
// factory hides which concrete implementation gets constructed.
package factory

import (
	"context"
	"fmt"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
	"github.com/luthermonson/shipmates/internal/runtime/codex"
)

// NewFromConfig returns a Runtime for the resolved name. Settings is the
// runtime-specific settings blob from config.Resolved.Settings; each
// runtime interprets its own keys.
//
// Codex construction takes StartOptions the settings blob can't reasonably
// carry (project working directory, containment posture); pass those via
// NewCodexWith instead.
func NewFromConfig(_ context.Context, name string, settings map[string]any) (runtime.Runtime, error) {
	switch name {
	case "claude":
		cfg := claude.Config{}
		if v, ok := settings["binary"].(string); ok {
			cfg.Binary = v
		}
		if raw, ok := settings["default_args"].([]any); ok {
			for _, v := range raw {
				if s, ok := v.(string); ok {
					cfg.DefaultArgs = append(cfg.DefaultArgs, s)
				}
			}
		}
		return claude.New(cfg), nil

	case "codex":
		return nil, fmt.Errorf("factory: codex must be constructed via NewCodexWith; NewFromConfig cannot start the app-server transport")

	default:
		return nil, fmt.Errorf("factory: unknown runtime %q", name)
	}
}

// NewCodexWith constructs a codex runtime with explicit transport options.
// Prefer this over NewFromConfig for codex.
func NewCodexWith(ctx context.Context, opts codexapp.StartOptions) (runtime.Runtime, error) {
	return codex.New(ctx, opts)
}

// Names returns the runtime names this factory knows about.
func Names() []string { return []string{"claude", "codex"} }
