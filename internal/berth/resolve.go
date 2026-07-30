package berth

import (
	"fmt"
	"log/slog"
	"path/filepath"

	"github.com/luthermonson/shipmates/internal/project"
)

// ResolveSpawnCWD returns the absolute working directory a persona's runtime
// session should run in, or "" for "no override — run at the project root",
// which is the behavior of a project that has never configured a berth.
//
// Resolution order:
//
//  1. An explicit `cwd:` crew override wins over the berth policy, so one
//     persona can be pointed at a custom directory without touching the
//     crew-wide berth default. A relative value is resolved against root.
//  2. Otherwise the `berth:` policy decides: off yields "", auto/require
//     create-or-reuse .shipmates/berths/<persona> via Ensure.
//
// The caller wires the result into runtime.SessionSpec.WorkingDir (or an
// exec.Cmd.Dir for a direct spawn). Nothing else moves: shipmates itself keeps
// running at the canonical root, so memory, policy and the install manifest
// are still resolved there — berths change where the child works, never where
// shipmates reads its own state from.
func ResolveSpawnCWD(persona string, cfg project.PersonaConfig) (string, error) {
	return ResolveSpawnCWDAt(".", persona, cfg)
}

// ResolveSpawnCWDAt is ResolveSpawnCWD for an explicit project root.
func ResolveSpawnCWDAt(root, persona string, cfg project.PersonaConfig) (string, error) {
	if cfg.CWD != "" {
		abs := cfg.CWD
		if !filepath.IsAbs(abs) {
			resolved, err := filepath.Abs(filepath.Join(root, cfg.CWD))
			if err != nil {
				return "", fmt.Errorf("resolve cwd override for %q: %w", persona, err)
			}
			abs = resolved
		}
		slog.Debug("spawning persona in configured cwd", "persona", persona, "cwd", abs)
		return abs, nil
	}

	path, err := EnsureAt(root, persona, ParsePolicy(cfg.Berth))
	if err != nil {
		return "", err
	}
	if path != "" {
		slog.Debug("spawning persona in berth", "persona", persona, "cwd", path)
	}
	return path, nil
}

// SessionFingerprint folds a resolved working directory into a persona's
// session fingerprint so a session is never *resumed* into a directory
// different from the one it was created in.
//
// This is the modern spelling of the v0.4.0 "berth only at session creation,
// never mid-session" guardrail. v0.4.0 persisted the creation cwd in the
// session marker and replayed it on resume; today the live-session path
// already treats cwd as part of session identity
// (livesession.Manager.StartIdle mixes it into the continuity fingerprint), so
// the same rule is enforced the same way here: changing a persona's berth
// drifts its fingerprint, which auto-freshes into a new session created in the
// new directory rather than resuming an old one somewhere else.
//
// An empty cwd returns base unchanged, so a project with no berths configured
// keeps byte-identical fingerprints and nothing is auto-freshed by the mere
// presence of this code.
func SessionFingerprint(base, cwd string) string {
	if cwd == "" {
		return base
	}
	return project.SHA([]byte(base + "\x00cwd=" + filepath.Clean(cwd)))
}
