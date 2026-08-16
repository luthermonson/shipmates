package claude

import "github.com/luthermonson/shipmates/internal/personaname"

// validatePersonaName rejects names that could escape or alias persona-scoped
// paths, or smuggle an extra CLI flag into the child's argv. A persona name
// reaches two places where a hostile value matters:
//
//   - it is joined into a filesystem path (AgentPath → .claude/agents/<name>.md),
//     so "../escape" or "nested/name" must be refused;
//   - it is passed to the child process as the --agent flag value, so a leading
//     dash ("-rf", "--dangerously-skip-permissions") must be refused before it
//     can be read as another flag.
//
// Validation happens at the render/spawn chokepoints — RenderPersona,
// UninstallPersona, StartSession, ResumeSession — not only at the CLI
// boundary, because the runtime interface is a public seam.
//
// This file used to carry its own copy of the regexp, with a note saying it
// should collapse into shipmates' canonical validator once one landed on main.
// internal/personaname is that validator, and this is the collapse: one rule,
// one error message, nothing left here that can drift away from it.
func validatePersonaName(persona string) error {
	return personaname.Validate(persona)
}
