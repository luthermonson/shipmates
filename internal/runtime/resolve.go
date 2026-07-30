package runtime

import (
	"context"
	"fmt"
)

// Selector resolves a runtime name from the caller's environment
// (project + user config files) with an optional CLI override, then
// builds the corresponding runtime.Runtime.
//
// This is the single entrypoint shipmates commands use — they never touch
// internal/runtime/config or internal/runtime/factory directly. That way
// swapping the config source (env vars, hosted profile, etc.) is a
// one-file change.
//
// Selector is an interface so tests can stub without importing the
// concrete factory + config packages. The production selector lives in
// internal/runtime/env.
type Selector interface {
	// Select produces a live Runtime based on project + optional CLI
	// override.
	Select(ctx context.Context, projectDir, cliRuntime string) (Runtime, string, error)
}

// SelectorFunc adapts a plain function to the Selector interface.
type SelectorFunc func(ctx context.Context, projectDir, cliRuntime string) (Runtime, string, error)

// Select implements Selector.
func (f SelectorFunc) Select(ctx context.Context, projectDir, cliRuntime string) (Runtime, string, error) {
	return f(ctx, projectDir, cliRuntime)
}

// ErrNotConfigured is returned when config resolution succeeded but the
// chosen runtime can't be instantiated in the caller's context (e.g. codex
// needs an app-server transport that isn't available). Commands treat
// this as "point the operator at the docs" rather than a bug.
type ErrNotConfigured struct {
	Runtime string
	Reason  string
}

func (e *ErrNotConfigured) Error() string {
	return fmt.Sprintf("runtime %q not configured: %s", e.Runtime, e.Reason)
}
