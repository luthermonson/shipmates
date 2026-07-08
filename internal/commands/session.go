package commands

import "github.com/luthermonson/shipmates/internal/project"

// sessionLaunch delegates to project.SessionLaunch — kept as a thin wrapper
// so command call sites read the same while the logic is shared with the
// captain server (live/PTY mates resume the same long-term session).
func sessionLaunch(persona string, fresh bool) (cfg project.PersonaConfig, args []string, id, name, fp string) {
	return project.SessionLaunch(persona, fresh)
}
