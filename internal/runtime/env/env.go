// Package env provides the production Selector shipmates commands use to
// obtain a runtime.Runtime from the operator's config + CLI flags. It
// stitches internal/runtime/config and internal/runtime/factory together
// so command code depends only on runtime.Selector.
package env

import (
	"context"
	"fmt"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/factory"
)

// Selector is the default runtime.Selector implementation. Zero value is
// usable; New() is a convenience.
type Selector struct{}

// New returns a Selector that reads project + user config from the
// standard shipmates locations.
func New() *Selector { return &Selector{} }

// Select implements runtime.Selector.
func (s *Selector) Select(ctx context.Context, projectDir, cliRuntime string) (runtime.Runtime, string, error) {
	projectCfg, err := config.LoadProject(projectDir)
	if err != nil {
		return nil, "", fmt.Errorf("load project config: %w", err)
	}
	userCfg, err := config.LoadUser()
	if err != nil {
		return nil, "", fmt.Errorf("load user config: %w", err)
	}
	resolved, err := config.Resolve(cliRuntime, projectCfg, userCfg)
	if err != nil {
		return nil, "", fmt.Errorf("resolve runtime: %w", err)
	}
	rt, err := factory.NewFromConfig(ctx, resolved.Runtime, resolved.Settings)
	if err != nil {
		// Codex is a known "needs more" case; surface it as ErrNotConfigured
		// so commands can print an actionable message.
		if resolved.Runtime == "codex" {
			return nil, resolved.Source, &runtime.ErrNotConfigured{
				Runtime: "codex",
				Reason:  "codex requires an app-server transport; use factory.NewCodexWith with StartOptions",
			}
		}
		return nil, resolved.Source, err
	}
	return rt, resolved.Source, nil
}
