package claude

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime"
)

// AgentsDir is where Claude Code reads project-scoped subagents from.
const AgentsDir = ".claude/agents"

// AgentPath is a persona's subagent file, relative to the project root.
// Shipmates records this path in the install manifest, so it is the one
// spelling of the location: both InstallPersona and the CLI's manifest-aware
// installer resolve through it.
func AgentPath(name string) string {
	return filepath.Join(filepath.FromSlash(AgentsDir), name+".md")
}

// RenderPersona returns the exact bytes of a persona's Claude Code subagent
// file. It is the single source of the artifact's content: InstallPersona
// writes what it returns, and `shipmates add`/`update` reconcile what it
// returns against the install manifest so a hand-edited file is never
// clobbered.
//
// Only the Claude Code-relevant frontmatter fields are emitted (name,
// description, model, tools). Shipmates-only fields (byline, domainGlob,
// memoryDir, permissions, remoteControl, berth) are elided — they live in
// the canonical catalog and shipmates carries them out-of-band.
func RenderPersona(p runtime.PersonaSpec) ([]byte, error) {
	// The name becomes a path segment (AgentPath) and a --agent flag value,
	// so it is validated here at the render chokepoint, not just at the CLI
	// boundary — the runtime interface is a public seam.
	if err := project.ValidatePersonaName(p.Name); err != nil {
		return nil, fmt.Errorf("claude: %w", err)
	}
	var buf bytes.Buffer
	if err := writeClaudeAgent(&buf, p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// InstallPersona writes .claude/agents/<name>.md under projectDir.
//
// This is the runtime-interface entry point and it writes unconditionally.
// `shipmates add`/`update` deliberately do not call it: they reconcile
// RenderPersona's bytes through the install manifest instead, which is what
// preserves an operator's local edits. Both paths therefore produce
// byte-identical artifacts (see TestRenderPersonaMatchesInstallPersona).
func (r *Runtime) InstallPersona(_ context.Context, projectDir string, p runtime.PersonaSpec) error {
	content, err := RenderPersona(p)
	if err != nil {
		return err
	}
	dest := filepath.Join(projectDir, AgentPath(p.Name))
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0o644)
}

// UninstallPersona removes .claude/agents/<name>.md if present. Missing
// file is not an error — idempotent.
func (r *Runtime) UninstallPersona(_ context.Context, projectDir, name string) error {
	if err := project.ValidatePersonaName(name); err != nil {
		return fmt.Errorf("claude: %w", err)
	}
	err := os.Remove(filepath.Join(projectDir, AgentPath(name)))
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

// claudeTools maps shipmates' canonical capability names onto the tool names
// Claude Code actually recognizes. The mapping is load-bearing, not cosmetic:
// verified against claude 2.1.153, a subagent whose frontmatter lists
// `tools: [read, edit, bash]` starts with `"tools":[]` in its init frame —
// every unrecognized name is dropped, so the canonical spelling would have
// silently left the persona with no tools at all. `tools: [Read, Glob, Grep,
// Bash]` yields exactly those four.
//
// Lookup is case-sensitive, and deliberately so: canonical capabilities are
// lowercase by definition (see runtime.PersonaSpec) while Claude Code's tool
// names are CamelCase, so the two vocabularies cannot collide. Anything not
// in the map is passed through verbatim, which is what lets a persona name a
// Claude Code tool — or an MCP tool — directly.
var claudeTools = map[string][]string{
	"read":   {"Read", "Glob", "Grep"},
	"edit":   {"Read", "Edit", "Write"},
	"bash":   {"Bash"},
	"browse": {"WebFetch", "WebSearch"},
}

// translateCapabilities expands canonical capability names into Claude Code
// tool names, de-duplicated and order-preserving. An empty result means the
// frontmatter carries no `tools` key at all, which is what grants the
// persona Claude Code's full tool set — the right default, because shipmates
// governs what a tool may actually do through policy and the can_use_tool
// approval path, not by amputating the toolbox.
func translateCapabilities(caps []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range caps {
		names, ok := claudeTools[strings.TrimSpace(c)]
		if !ok {
			names = []string{c}
		}
		for _, n := range names {
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
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
		Tools:       translateCapabilities(p.Capabilities),
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
	// The body is the persona's system prompt, verbatim. Canonical bodies
	// arrive with the newline that followed their frontmatter still attached;
	// strip it so the artifact has exactly one blank line after the
	// frontmatter no matter which caller produced the spec.
	body := p.SystemPrompt
	if strings.TrimSpace(body) == "" {
		body = fmt.Sprintf("# %s\n\n%s\n", p.Name, p.Description)
	}
	body = strings.TrimLeft(body, "\n")
	if !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	_, err := io.WriteString(w, body)
	return err
}
