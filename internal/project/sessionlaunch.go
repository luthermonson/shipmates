package project

import "log/slog"

// SessionLaunch resolves a persona's session-identity args for a claude
// spawn — the single source of truth shared by ask/drain/open/fanout (via
// the commands package) AND the captain server's live/PTY mates, so every
// surface continues ONE long-term conversation per shipmate instead of
// forking per-surface histories.
//
// It resumes the tracked session by its UUID (unambiguous), or creates a
// fresh one. It creates fresh when: fresh is requested, the persona's config
// drifted (model/effort baked at creation), OR there's no tracked session ID
// to resume — which sidesteps the `--resume <name>` ambiguity error when
// multiple sessions share a name.
//
// Returns the resolved config, the identity args (no -p prefix), the session
// id and name, and the config fingerprint to record (WriteSessionMeta) after
// a successful launch.
func SessionLaunch(persona string, fresh bool) (cfg PersonaConfig, args []string, id, name, fp string) {
	name = SessionName(persona)
	cfg, _ = ResolvePersonaConfig(persona)
	fp = cfg.Fingerprint()

	meta, have := ReadSessionMeta(persona)
	if have && !fresh && meta.ConfigHash != "" && meta.ConfigHash != fp {
		fresh = true
		slog.Info("persona config changed since last session; starting fresh", "persona", persona)
	}

	if have && !fresh && meta.ID != "" {
		id = meta.ID
		args = []string{"--resume", id, "--agent", persona}
	} else {
		id = NewUUID()
		args = []string{"--session-id", id, "--name", name, "--agent", persona}
	}
	return cfg, args, id, name, fp
}
