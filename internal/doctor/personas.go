package doctor

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
)

// checkPersonas resolves every installed fleet persona and flags (Warn) any
// whose checkout tries to set operator-only keys — backend/command/cwd/
// dangerouslySkipPermissions, or a permission mode outside the repo allowlist
// (e.g. bypassPermissions). It reuses project.ResolvePersonaConfig, which
// already refuses those keys and records them in PersonaConfig.Refused, so
// doctor surfaces the same refusal the spawn path would hit — proactively,
// before a mate is launched.
//
// A refusal is never a Fail: the settings are ignored at spawn, so the persona
// still runs safely; it is a Warn so the operator learns the setting had no
// effect and where it must live instead.
func checkPersonas(_ Env) []Result {
	matches, err := filepath.Glob(filepath.Join(project.AgentsDir, "*.md"))
	if err != nil {
		return []Result{{
			Name: "installed personas", Group: "Personas", Status: Warn,
			Detail: "could not scan " + project.AgentsDir + ": " + oneLine(err.Error()),
		}}
	}

	var names []string
	for _, m := range matches {
		if project.IsFleetPersonaFile(m) {
			names = append(names, strings.TrimSuffix(filepath.Base(m), ".md"))
		}
	}
	sort.Strings(names)

	if len(names) == 0 {
		return []Result{{
			Name: "installed personas", Group: "Personas", Status: OK,
			Detail: "none installed in " + project.AgentsDir + " — run `shipmates init` or `shipmates add`",
		}}
	}

	var out []Result
	for _, name := range names {
		cfg, err := project.ResolvePersonaConfig(name)
		if err != nil {
			out = append(out, Result{
				Name: "persona " + name, Group: "Personas", Status: Warn,
				Detail: "could not resolve: " + oneLine(err.Error()),
			})
			continue
		}
		if len(cfg.Refused) > 0 {
			out = append(out, Result{
				Name: "persona " + name, Group: "Personas", Status: Warn,
				Detail: "checkout tries operator-only key(s): " + cfg.RefusedSummary() + " — ignored at spawn (a checkout may not set backend/command/cwd/dangerouslySkipPermissions or a mode outside the allowlist)",
				Hint:   "move operator-only settings to ~/.shipmates/personas.yaml",
			})
			continue
		}
		out = append(out, Result{
			Name: "persona " + name, Group: "Personas", Status: OK,
			Detail: "resolves cleanly; no operator-only keys attempted",
		})
	}
	return out
}
