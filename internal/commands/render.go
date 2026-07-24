package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// frontmatter is the subset of a persona's YAML frontmatter that thin targets
// care about. Memory dynamics (memoryDir, permissions, remoteControl) are
// intentionally omitted — thin targets have no memory loop.
type frontmatter struct {
	Name        string
	Description string
	Byline      string
	DomainGlob  []string
}

// Render emits a thin-target version of a persona.
//
// Thin targets are one-way renders for tools that do not read Codex agents
// subagent files natively. They degrade gracefully: the memory-load
// instructions are dropped (no memory dynamics) and the body is condensed.
//
// By default it prints to stdout; with --write it writes to each target's
// canonical destination instead.
func Render(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "render",
		Usage:     "render a persona for a target (agents-md|codex|cursor|windsurf)",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "target", Required: true},
			&cli.BoolFlag{Name: "write", Usage: "write to the target's canonical file instead of stdout"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name := c.Args().First()
			if name == "" {
				return errors.New("usage: shipmates render <persona> --target <agents-md|cursor|windsurf>")
			}
			if !cat.Has(name) {
				return fmt.Errorf("unknown persona %q", name)
			}

			raw, err := cat.AgentFile(name)
			if err != nil {
				return fmt.Errorf("read agent file: %w", err)
			}

			fm, body := splitPersona(raw)
			if fm.Name == "" {
				fm.Name = name
			}

			target := c.String("target")
			var out string
			switch target {
			case "agents-md":
				out = renderAgentsMD(fm, body)
			case "codex":
				out = renderCodex(fm, body)
			case "cursor":
				out = renderCursor(fm, body)
			case "windsurf":
				out = renderWindsurf(fm, body)
			default:
				return fmt.Errorf("unknown target %q (want: agents-md|codex|cursor|windsurf)", target)
			}

			if c.Bool("write") {
				return writeRender(target, name, out)
			}
			fmt.Print(out)
			return nil
		},
	}
}

// writeRender persists a rendered thin target to its canonical destination.
//   - codex:     .codex/agents/<persona>.toml — standalone custom agent, overwritten.
//   - cursor:    .cursor/rules/<persona>.mdc — standalone per-persona rule, overwritten.
//   - agents-md: a marked section in ./AGENTS.md.
//   - windsurf:  a marked section in ./.windsurf/rules.md.
//
// The append/update targets are idempotent: re-rendering replaces the persona's
// own marked block and leaves everything else untouched.
func writeRender(target, persona, content string) error {
	switch target {
	case "codex":
		path := filepath.Join(".codex", "agents", persona+".toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		slog.Info("wrote Codex custom agent", "persona", persona, "path", path)
		return nil
	case "cursor":
		path := filepath.Join(".cursor", "rules", persona+".mdc")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		slog.Info("wrote thin target", "target", target, "persona", persona, "path", path)
		return nil
	case "agents-md":
		return upsertMarkedSection("AGENTS.md", persona, content)
	case "windsurf":
		return upsertMarkedSection(filepath.Join(".windsurf", "rules.md"), persona, content)
	default:
		return fmt.Errorf("unknown target %q (want: agents-md|codex|cursor|windsurf)", target)
	}
}

// upsertMarkedSection inserts or replaces a per-persona block delimited by
// HTML-comment markers within path, creating the file (and parent dirs) if
// needed. Content outside the markers is preserved verbatim.
func upsertMarkedSection(path, persona, content string) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
	}

	startMarker := fmt.Sprintf("<!-- shipmates:%s:start -->", persona)
	endMarker := fmt.Sprintf("<!-- shipmates:%s:end -->", persona)
	block := startMarker + "\n" + strings.TrimRight(content, "\n") + "\n" + endMarker + "\n"

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	text := strings.ReplaceAll(string(existing), "\r\n", "\n")

	var updated string
	start := strings.Index(text, startMarker)
	end := strings.Index(text, endMarker)
	switch {
	case start != -1 && end != -1 && end > start:
		updated = text[:start] + block + text[end+len(endMarker):]
		updated = strings.TrimRight(updated, "\n") + "\n"
	case text == "":
		updated = block
	default:
		updated = strings.TrimRight(text, "\n") + "\n\n" + block
	}

	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	slog.Info("wrote thin target", "persona", persona, "path", path)
	return nil
}

// splitPersona separates a persona file into its parsed frontmatter and body.
func splitPersona(raw []byte) (frontmatter, string) {
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.TrimLeft(text, "\n")

	if !strings.HasPrefix(text, "---\n") {
		return frontmatter{}, strings.TrimSpace(text)
	}

	rest := text[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end == -1 {
		return frontmatter{}, strings.TrimSpace(text)
	}

	fmText := rest[:end]
	body := rest[end+len("\n---"):]
	if nl := strings.IndexByte(body, '\n'); nl != -1 {
		body = body[nl+1:]
	} else {
		body = ""
	}

	return parseFrontmatter(fmText), strings.TrimSpace(body)
}

// parseFrontmatter does a minimal, dependency-free parse of the flat scalar
// fields and the one block-sequence (domainGlob) that thin targets use.
func parseFrontmatter(s string) frontmatter {
	var fm frontmatter
	lines := strings.Split(s, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if line != strings.TrimLeft(line, " \t") {
			continue
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.TrimSpace(val)

		switch key {
		case "name":
			fm.Name = unquoteScalar(val)
		case "description":
			fm.Description = unquoteScalar(val)
		case "byline":
			fm.Byline = unquoteScalar(val)
		case "domainGlob":
			if val != "" {
				fm.DomainGlob = parseFlowList(val)
				break
			}
			for j := i + 1; j < len(lines); j++ {
				item := strings.TrimSpace(lines[j])
				if strings.HasPrefix(item, "- ") {
					fm.DomainGlob = append(fm.DomainGlob, unquoteScalar(strings.TrimSpace(item[2:])))
					i = j
					continue
				}
				if item == "" {
					i = j
					continue
				}
				break
			}
		}
	}
	return fm
}

func unquoteScalar(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

func parseFlowList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	var out []string
	for _, part := range strings.Split(v, ",") {
		if p := unquoteScalar(strings.TrimSpace(part)); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// condenseBody trims memory-centric sections out of the persona body so thin
// targets carry only the static guidance.
func condenseBody(body string) string {
	lines := strings.Split(body, "\n")
	var out []string
	skipSection := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		if isHeading(trimmed) {
			skipSection = headingIsMemory(trimmed)
			if skipSection {
				continue
			}
		}
		if skipSection {
			continue
		}

		low := strings.ToLower(trimmed)
		if strings.HasPrefix(trimmed, "-") &&
			(strings.Contains(low, "read your memory") ||
				strings.Contains(low, "read everything in `.shipmates/memory") ||
				strings.Contains(low, "session start, read")) {
			continue
		}

		out = append(out, line)
	}

	condensed := strings.Join(out, "\n")
	for strings.Contains(condensed, "\n\n\n") {
		condensed = strings.ReplaceAll(condensed, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(condensed)
}

func isHeading(line string) bool { return strings.HasPrefix(line, "#") }

func headingIsMemory(line string) bool {
	h := strings.ToLower(strings.TrimSpace(strings.TrimLeft(line, "#")))
	return h == "memory" || strings.HasPrefix(h, "memory ")
}

func renderAgentsMD(fm frontmatter, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", fm.Name)
	if fm.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", fm.Description)
	}
	if len(fm.DomainGlob) > 0 {
		fmt.Fprintf(&b, "**Applies to:** %s\n\n", strings.Join(fm.DomainGlob, ", "))
	}
	b.WriteString(condenseBody(body))
	b.WriteString("\n")
	return b.String()
}

// renderCodex emits a project-scoped custom agent. Unlike the generic
// AGENTS.md target, the persona remains independently addressable by Codex
// subagent workflows and carries Shipmates' file-backed memory convention.
func renderCodex(fm frontmatter, body string) string {
	name := fm.Name
	if name == "" {
		name = "shipmate"
	}
	description := fm.Description
	if description == "" {
		description = "Shipmates persona"
	}

	instructions := strings.TrimSpace(body)
	instructions += "\n\n## Persistent Memory\n\n" +
		"At the start of each task, read the relevant files under `.shipmates/memory/" + name + "/`. " +
		"Record durable project decisions, verified constraints, and reusable findings there when they would help a later task."
	// Prepend the Ship's Articles reminder. Idempotent — re-composing a
	// persona replaces the marker-delimited block instead of stacking new
	// copies.
	instructions = project.PrependArticlesBlock(instructions)

	var b strings.Builder
	fmt.Fprintf(&b, "name = %s\n", strconv.Quote(name))
	fmt.Fprintf(&b, "description = %s\n", strconv.Quote(description))
	if len(fm.DomainGlob) > 0 {
		fmt.Fprintf(&b, "# Primary domains: %s\n", strings.Join(fm.DomainGlob, ", "))
	}
	fmt.Fprintf(&b, "developer_instructions = %s\n", strconv.Quote(instructions))
	return b.String()
}

func renderCursor(fm frontmatter, body string) string {
	var b strings.Builder
	b.WriteString("---\n")
	if fm.Description != "" {
		fmt.Fprintf(&b, "description: %s\n", fm.Description)
	}
	if len(fm.DomainGlob) > 0 {
		fmt.Fprintf(&b, "globs: %s\n", strings.Join(fm.DomainGlob, ","))
	}
	b.WriteString("alwaysApply: false\n")
	b.WriteString("---\n\n")
	fmt.Fprintf(&b, "# %s\n\n", fm.Name)
	b.WriteString(condenseBody(body))
	b.WriteString("\n")
	return b.String()
}

func renderWindsurf(fm frontmatter, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", fm.Name)
	if fm.Description != "" {
		fmt.Fprintf(&b, "%s\n\n", fm.Description)
	}
	if len(fm.DomainGlob) > 0 {
		fmt.Fprintf(&b, "_Globs: %s_\n\n", strings.Join(fm.DomainGlob, ", "))
	}
	b.WriteString(condenseBody(body))
	b.WriteString("\n")
	return b.String()
}
