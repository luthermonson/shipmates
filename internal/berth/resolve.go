package berth

import (
	"log/slog"

	"github.com/luthermonson/shipmates/internal/project"
)

// ResolveSpawnCWD returns the absolute cwd a spawn should use for a persona,
// combining the persona's resolved config with its stored session meta:
//
//   - If a session already exists (resume path), the stored CWD wins — even
//     when it's empty (a pre-berth session stays at repo root). This is the
//     "berth only at session creation" guardrail: existing sessions keep
//     their creation-cwd until explicitly --fresh'ed.
//   - If this launch is CREATING a new session, use the frontmatter CWD
//     override when set; otherwise consult the berth policy and (for auto/
//     require) create-or-reuse the berth via Ensure. PolicyOff yields "".
//
// An empty return means "no cmd.Dir override — spawn at the shipmates process
// cwd," which is today's behavior at the repo root. The caller wires the
// result straight into exec.Cmd.Dir.
//
// creating is the flag SessionLaunch returns: true when this launch mints a
// fresh session (--session-id), false when it resumes an existing one
// (--resume). It's the seam that keeps the guardrail honest.
func ResolveSpawnCWD(persona string, cfg project.PersonaConfig, creating bool) (string, error) {
	// Resume: preserve the session's creation-cwd exactly. This is R1's
	// mitigation for the session-resume open blocker.
	if !creating {
		return project.ResumeCWD(persona), nil
	}

	// Creating a new session: an explicit cwd override in frontmatter wins
	// over the berth policy — a persona can opt out of berths (or point at a
	// custom dir) without touching the crew-wide default.
	if cfg.CWD != "" {
		return cfg.CWD, nil
	}

	policy := ParsePolicy(cfg.Berth)
	path, err := Ensure(persona, policy)
	if err != nil {
		return "", err
	}
	if path != "" {
		slog.Debug("spawning persona in berth", "persona", persona, "cwd", path)
	}
	return path, nil
}
