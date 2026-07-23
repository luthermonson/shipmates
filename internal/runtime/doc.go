// Package runtime declares the AI-runtime abstraction shipmates uses to talk
// to Claude Code, Codex, and future coding agents through one seam.
//
// # Migration state
//
// Phase 0 (this commit): the interface + types + design doc. No
// implementations wired.
//
// Subsequent phases restore Claude Code as an implementation, adapt the
// existing codexapp.Adapter to satisfy the interface, add config-driven
// selection, and gate cgroup containment behind //go:build linux. See
// docs/runtime-interface-plan.md for the full plan and phase list.
package runtime
