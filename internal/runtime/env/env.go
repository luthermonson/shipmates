// Package env is the production runtime.Selector: it stitches
// internal/runtime/config and internal/runtime/factory together so shipmates
// command code depends only on this one package.
//
// Commands go through a Selector rather than reaching into config and factory
// themselves, so that changing where a runtime selection comes from — env
// vars, a hosted profile, a fleet policy — is a change to this file and
// nothing else.
package env

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/factory"
)

// Selector resolves the runtime for an invocation and, on request, builds it.
// The zero value is usable and reads config from the standard locations.
type Selector struct {
	// UserHome overrides where ~/.shipmates/config.yaml is looked for. Empty
	// means the real home directory. Tests set it; production does not.
	UserHome string
}

// New returns a Selector reading project and user config from the standard
// shipmates locations.
func New() *Selector { return &Selector{} }

var _ runtime.Selector = (*Selector)(nil)

// Selection is the outcome of resolving a runtime name, without building
// anything. Commands that only need to know WHICH runtime is in play — to
// decide which artifacts to write, or to report the choice — use this and skip
// starting a transport.
type Selection struct {
	// Runtime is the resolved runtime name.
	Runtime string
	// Source says where the name came from: config.SourceOverride, "project
	// config", "user config" or "default". Worth printing, because "why is
	// this project using codex" is otherwise an archaeology exercise.
	Source string
	// Containment is the operator's containment posture. From user config
	// only — a project checkout cannot influence it.
	Containment config.Containment
	// Settings is the per-runtime settings blob, from user config only.
	Settings map[string]any
	// IgnoredProjectKeys lists keys the project's .shipmates/config.yaml set
	// that a checkout is not trusted to supply. Non-empty means something was
	// deliberately discarded and the operator should hear about it.
	IgnoredProjectKeys []string
}

// Resolve determines which runtime an invocation should use, applying
// precedence: cliRuntime > project config > user config > default.
//
// It warns once about anything a project config tried to set that it was not
// trusted to. Callers get the list too, so a command can surface it however
// it likes.
func (s *Selector) Resolve(projectDir, cliRuntime string) (Selection, error) {
	projectCfg, err := config.LoadProject(projectDir)
	if err != nil {
		return Selection{}, fmt.Errorf("load project runtime config: %w", err)
	}
	userCfg, err := config.LoadUser(s.UserHome)
	if err != nil {
		return Selection{}, fmt.Errorf("load user runtime config: %w", err)
	}
	r, err := config.Resolve(cliRuntime, projectCfg, userCfg)
	if err != nil {
		return Selection{}, fmt.Errorf("resolve runtime: %w", err)
	}
	if len(r.IgnoredProjectKeys) > 0 {
		slog.Warn("ignoring keys in this project's .shipmates/config.yaml: a checkout may select the runtime but not what it executes or how it is contained",
			"path", config.ProjectPath(projectDir),
			"ignored", r.IgnoredProjectKeys)
	}
	return Selection{
		Runtime:            r.Runtime,
		Source:             r.Source,
		Containment:        r.Containment,
		Settings:           r.Settings,
		IgnoredProjectKeys: r.IgnoredProjectKeys,
	}, nil
}

// Select resolves the runtime and builds it, returning the live Runtime and
// the source of the selection. It implements runtime.Selector.
//
// Building can fail for ordinary environmental reasons — codex needs an
// app-server, openai needs an endpoint — and the factory reports those as
// *runtime.ErrNotConfigured so a command can point the operator at the docs
// instead of reporting a bug.
func (s *Selector) Select(ctx context.Context, projectDir, cliRuntime string) (runtime.Runtime, string, error) {
	return s.SelectIn(ctx, projectDir, "", cliRuntime)
}

// SelectIn is Select with an explicit working directory for the runtime's tool
// calls, which is where a persona's berth lands. An empty workingDir means
// projectDir.
func (s *Selector) SelectIn(ctx context.Context, projectDir, workingDir, cliRuntime string) (runtime.Runtime, string, error) {
	sel, err := s.Resolve(projectDir, cliRuntime)
	if err != nil {
		return nil, "", err
	}
	rt, err := factory.New(ctx, config.Resolved{
		Runtime:     sel.Runtime,
		Settings:    sel.Settings,
		Containment: sel.Containment,
		Source:      sel.Source,
	}, factory.Options{ProjectDir: projectDir, WorkingDir: workingDir})
	if err != nil {
		return nil, sel.Source, err
	}
	return rt, sel.Source, nil
}
