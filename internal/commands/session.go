package commands

import (
	"github.com/luthermonson/shipmates/internal/berth"
	"github.com/luthermonson/shipmates/internal/project"
)

// sessionLaunch delegates to project.SessionLaunch — kept as a thin wrapper
// so command call sites read the same while the logic is shared with the
// captain server (live/PTY mates resume the same long-term session).
//
// The returned cwd is the resolved cmd.Dir for the child claude process:
// empty means "spawn at the shipmates process cwd" (today's behavior), a
// non-empty value is a berth path (or a frontmatter cwd override) — see
// berth.ResolveSpawnCWD. Resumes preserve their creation-cwd; only fresh
// sessions consult the berth policy.
func sessionLaunch(persona string, fresh bool) (cfg project.PersonaConfig, args []string, id, name, fp, cwd string) {
	var creating bool
	cfg, args, id, name, fp, creating = project.SessionLaunch(persona, fresh)
	cwd, _ = berth.ResolveSpawnCWD(persona, cfg, creating)
	return cfg, args, id, name, fp, cwd
}
