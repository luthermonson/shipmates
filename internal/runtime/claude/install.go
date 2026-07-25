package claude

import (
	"bytes"
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

// MemoryHookCommand is the command the SessionStart hook runs. It doubles
// as the marker that makes installation idempotent.
const MemoryHookCommand = "shipmates hook load-memory"

// InstallMemoryHook implements runtime.Runtime by delegating to
// InstallMemoryHookAt, which callers that only need the file (persona
// install, project init) can use without constructing a Runtime.
func (r *Runtime) InstallMemoryHook(_ context.Context, projectDir string) error {
	return InstallMemoryHookAt(projectDir)
}

// InstallMemoryHookAt writes a SessionStart hook into
// <projectDir>/.claude/settings.json that runs `shipmates hook load-memory`
// before every session. That is what lets each persona read its durable
// memory automatically instead of the operator remembering to paste it.
//
// The shape is Claude Code's matcher-group form, verified against claude
// 2.1.153 with `--include-hook-events`:
//
//	{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"…"}]}]}}
//
// producing hook_started/hook_response frames named "SessionStart:startup".
// The flatter {"SessionStart":[{"type":"command",…}]} spelling parses
// without complaint and is then silently ignored — no hook event, no
// injected context — so any such entry we previously wrote is migrated into
// a group rather than left as dead configuration. The matcher is omitted on
// purpose: memory should load on resumed sessions too, not just fresh ones.
//
// Existing settings are merged, never replaced: other hook events, other
// SessionStart groups, and every unrelated key are preserved byte-for-byte
// in value. Re-running is a no-op once the hook is present, so it is safe to
// call on every install and update.
func InstallMemoryHookAt(projectDir string) error {
	dir := filepath.Join(projectDir, ".claude")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	settingsPath := filepath.Join(dir, "settings.json")

	// Load existing settings if present.
	var settings map[string]any
	if raw, err := os.ReadFile(settingsPath); err == nil {
		// An empty or whitespace-only file is what an interrupted write or an
		// editor can leave behind; treat it as absent rather than a parse
		// error the operator has to clear by hand.
		if len(bytes.TrimSpace(raw)) > 0 {
			if err := json.Unmarshal(raw, &settings); err != nil {
				return fmt.Errorf("claude: parse %s: %w", settingsPath, err)
			}
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

	groups := make([]any, 0, len(sessionStart)+1)
	installed := false
	for _, entry := range sessionStart {
		m, ok := entry.(map[string]any)
		if !ok {
			groups = append(groups, entry)
			continue
		}
		// Drop a legacy flat entry of ours; it never fired, and re-adding it
		// below in group form is the migration.
		if cmd, _ := m["command"].(string); cmd == MemoryHookCommand && m["hooks"] == nil {
			continue
		}
		if groupHasCommand(m, MemoryHookCommand) {
			installed = true
		}
		groups = append(groups, entry)
	}
	if !installed {
		groups = append(groups, map[string]any{
			"hooks": []any{map[string]any{"type": "command", "command": MemoryHookCommand}},
		})
	}
	hooks["SessionStart"] = groups
	settings["hooks"] = hooks

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(settingsPath, append(out, '\n'), 0o644)
}

// groupHasCommand reports whether a SessionStart matcher group already runs
// the given command.
func groupHasCommand(group map[string]any, command string) bool {
	inner, _ := group["hooks"].([]any)
	for _, h := range inner {
		m, ok := h.(map[string]any)
		if !ok {
			continue
		}
		if cmd, _ := m["command"].(string); cmd == command {
			return true
		}
	}
	return false
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
