package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/runtime"
)

// InstallPersona writes .claude/agents/<name>.md under projectDir. It emits
// only the Claude Code-relevant frontmatter fields (name, description,
// model, tools). Shipmates-only fields (byline, domainGlob, memoryDir,
// permissions, remoteControl, berth) are elided — they live in the
// canonical catalog and shipmates carries them out-of-band.
func (r *Runtime) InstallPersona(_ context.Context, projectDir string, p runtime.PersonaSpec) error {
	if p.Name == "" {
		return fmt.Errorf("claude: persona name required")
	}
	dir := filepath.Join(projectDir, ".claude", "agents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	dest := filepath.Join(dir, p.Name+".md")
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	return writeClaudeAgent(f, p)
}

// UninstallPersona removes .claude/agents/<name>.md if present. Missing
// file is not an error — idempotent.
func (r *Runtime) UninstallPersona(_ context.Context, projectDir, name string) error {
	if name == "" {
		return fmt.Errorf("claude: persona name required")
	}
	err := os.Remove(filepath.Join(projectDir, ".claude", "agents", name+".md"))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// InstallMemoryHook writes a SessionStart hook block into
// .claude/settings.json that runs `shipmates hook load-memory` before every
// persona session. This is what lets each persona "read its memory first"
// automatically — the operator doesn't have to remember to add the hook.
//
// The hook block is idempotent: re-running InstallMemoryHook adds the
// entry only if it isn't already present, so this is safe to call
// unconditionally at every `shipmates init`.
func (r *Runtime) InstallMemoryHook(_ context.Context, projectDir string) error {
	dir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	settingsPath := filepath.Join(dir, "settings.json")

	// Load existing settings if present.
	var settings map[string]any
	if raw, err := os.ReadFile(settingsPath); err == nil {
		if err := json.Unmarshal(raw, &settings); err != nil {
			return fmt.Errorf("claude: parse %s: %w", settingsPath, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if settings == nil {
		settings = map[string]any{}
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	sessionStart, _ := hooks["SessionStart"].([]any)
	// Guard against duplicate installs by checking for our marker command.
	const marker = "shipmates hook load-memory"
	for _, entry := range sessionStart {
		if m, ok := entry.(map[string]any); ok {
			if cmd, _ := m["command"].(string); cmd == marker {
				return nil // already installed
			}
		}
	}
	sessionStart = append(sessionStart, map[string]any{
		"type":    "command",
		"command": marker,
	})
	hooks["SessionStart"] = sessionStart
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, out, 0o644)
}

// writeClaudeAgent emits the frontmatter + body Claude Code expects.
func writeClaudeAgent(w io.Writer, p runtime.PersonaSpec) error {
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
