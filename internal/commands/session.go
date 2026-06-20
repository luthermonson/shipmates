package commands

import (
	"log/slog"

	"github.com/luthermonson/shipmates/internal/project"
)

// sessionLaunch resolves a persona's session-identity args for a claude spawn,
// the single source of truth shared by dispatch (ask/drain), open, and fanout.
//
// It resumes the tracked session by its UUID (unambiguous), or creates a fresh
// one. It creates fresh when: fresh is requested, the persona's config drifted
// (model/effort baked at creation), OR there's no tracked session ID to resume
// — which sidesteps the `--resume <name>` ambiguity error when multiple sessions
// share a name.
//
// Returns the resolved config, the identity args (no -p prefix), the session id
// and name, and the config fingerprint to record after a successful run.
func sessionLaunch(persona string, fresh bool) (cfg project.PersonaConfig, args []string, id, name, fp string) {
	name = project.SessionName(persona)
	cfg, _ = project.ResolvePersonaConfig(persona)
	fp = cfg.Fingerprint()

	meta, have := project.ReadSessionMeta(persona)
	if have && !fresh && meta.ConfigHash != "" && meta.ConfigHash != fp {
		fresh = true
		slog.Info("persona config changed since last session; starting fresh", "persona", persona)
	}

	if have && !fresh && meta.ID != "" {
		id = meta.ID
		args = []string{"--resume", id, "--agent", persona}
	} else {
		id = project.NewUUID()
		args = []string{"--session-id", id, "--name", name, "--agent", persona}
	}
	return cfg, args, id, name, fp
}
