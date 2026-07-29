package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// InstallPersona writes .codex/agents/<name>.md under projectDir. Codex's
// agent frontmatter is a subset of the canonical spec (name, description,
// model, tools); shipmates-only fields are elided.
func (r *Runtime) InstallPersona(_ context.Context, projectDir string, p runtime.PersonaSpec) error {
	// The name becomes a path segment below, so it is validated here rather
	// than trusted from the caller — the runtime interface is a public seam.
	if err := project.ValidatePersonaName(p.Name); err != nil {
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
	if err := project.ValidatePersonaName(name); err != nil {
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
// `shipmates add`/`update` already render into .codex/agents/<persona>.md.
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
	_, err := io.WriteString(w, body)
	return err
}
