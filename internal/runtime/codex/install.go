package codex

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// InstallPersona writes .codex/agents/<name>.md under projectDir. Codex's
// agent frontmatter is a subset of the canonical spec (name, description,
// model, tools); shipmates-only fields are elided.
func (r *Runtime) InstallPersona(_ context.Context, projectDir string, p runtime.PersonaSpec) error {
	if p.Name == "" {
		return fmt.Errorf("codex: persona name required")
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
	if name == "" {
		return fmt.Errorf("codex: persona name required")
	}
	err := os.Remove(filepath.Join(projectDir, ".codex", "agents", name+".md"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InstallMemoryHook wires codex's session-start memory injection AND the
// Brig reminder ("you are bound by the Ship's Articles at
// .shipmates/ARTICLES.md; violating any rule sends you to the brig.").
//
// TODO(follow-up): codex has a distinct mechanism from Claude's
// SessionStart hook (its agent config is JSON-driven with different fields).
// The exact wiring lives in the codexapp adapter and the branch's
// installer code — pulling it into a stable per-runtime seam is a
// follow-up commit. For now the memory + Brig reminders are covered at
// the prompt layer via project.PrependArticlesBlock, which is invoked
// during renderCodex; this method is a no-op so shipmates init can call
// InstallMemoryHook on both runtimes uniformly.
func (r *Runtime) InstallMemoryHook(context.Context, string) error {
	return nil
}

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
