package claude

import (
	"fmt"
	"regexp"
)

// personaNameRE is the one spelling of a legal persona name in this package.
// Lowercase-first, then lowercase letters, digits, hyphens and underscores —
// nothing that can be a path separator, a parent-directory hop, a leading
// flag dash, or whitespace.
var personaNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

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
// NOTE: shipmates has one canonical validator, project.ValidatePersonaName,
// and this function deliberately mirrors its regexp and its error text so the
// two cannot disagree. It is duplicated here only because that function does
// not exist on this branch's base (`main` carries no ValidatePersonaName in
// internal/project); once it lands, this file should become a one-line
// delegation to it.
func validatePersonaName(persona string) error {
	if !personaNameRE.MatchString(persona) {
		return fmt.Errorf("invalid persona %q (use lowercase letters, digits, hyphens, or underscores)", persona)
	}
	return nil
}
