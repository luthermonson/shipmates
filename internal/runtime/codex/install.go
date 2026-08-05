package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// personaNameRE bounds a persona name to something that cannot escape or alias a
// persona-scoped path.
//
// This duplicates project.ValidatePersonaName's rule rather than calling it:
// internal/project does not export that helper on this branch, and this package
// must not be the one to add it. When the shared helper lands, replace this with
// a call to it — the pattern is deliberately identical so the swap is mechanical.
var personaNameRE = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

func validatePersonaName(persona string) error {
	if !personaNameRE.MatchString(persona) {
		return fmt.Errorf("invalid persona %q (use lowercase letters, digits, hyphens, or underscores)", persona)
	}
	return nil
}

// InstallPersona writes .codex/agents/<name>.md under projectDir. Codex's
// agent frontmatter is a subset of the canonical spec (name, description,
// model, tools); shipmates-only fields are elided.
func (r *Runtime) InstallPersona(_ context.Context, projectDir string, p runtime.PersonaSpec) error {
	// The name becomes a path segment below, so it is validated here rather
	// than trusted from the caller — the runtime interface is a public seam.
	if err := validatePersonaName(p.Name); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	dir := filepath.Join(projectDir, ".codex", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, p.Name+".md")
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeCodexAgent(f, p)
}

// UninstallPersona removes .codex/agents/<name>.md if present.
func (r *Runtime) UninstallPersona(_ context.Context, projectDir, name string) error {
	if err := validatePersonaName(name); err != nil {
		return fmt.Errorf("codex: %w", err)
	}
	err := os.Remove(filepath.Join(projectDir, ".codex", "agents", name+".md"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InstallMemoryHook implements runtime.Runtime by delegating to
// InstallMemoryHookAt.
func (r *Runtime) InstallMemoryHook(_ context.Context, projectDir string) error {
	return InstallMemoryHookAt(projectDir)
}

// InstallMemoryHookAt is codex's half of the "read memory on session start"
// seam. Codex has no equivalent of Claude Code's SessionStart hook: memory
// reaches a codex turn through the persona's developer instructions, which
// persona install/update already renders into .codex/agents/<persona>.md.
// There is therefore nothing to install, and — importantly — nothing is
// written: a codex-only project must not grow a .claude/ directory.
//
// It is a no-op rather than ErrUnsupported so callers can install the hook
// for whichever runtime is configured without special-casing codex.
func InstallMemoryHookAt(string) error { return nil }

func writeCodexAgent(w io.Writer, p runtime.PersonaSpec) error {
	type frontmatter struct {
		Name        string   `yaml:"name"`
		Description string   `yaml:"description,omitempty"`
		Model       string   `yaml:"model,omitempty"`
		Tools       []string `yaml:"tools,omitempty"`
	}
	fm := frontmatter{
		Name:        p.Name,
		Description: p.Description,
		Model:       p.Model,
		Tools:       p.Capabilities,
	}
	if _, err := io.WriteString(w, "---\n"); err != nil {
		return err
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(fm); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	if _, err := io.WriteString(w, "---\n\n"); err != nil {
		return err
	}
	body := p.SystemPrompt
	if body == "" {
		body = fmt.Sprintf("# %s\n\n%s\n", p.Name, p.Description)
	}
	// The Ship's Articles reminder rides the persona body — codex has no
	// session-start hook seam, so the prompt layer IS the artifact. Spliced
	// idempotently per the operator's brig posture (user config only).
	body = brig.SplicePrompt(body, brig.PromptBlock(brig.Load("")))
	_, err := io.WriteString(w, body)
	return err
}
