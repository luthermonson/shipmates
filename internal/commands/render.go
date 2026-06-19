package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthermonson/shipmates/internal/catalog"
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
// Thin targets are one-way renders for tools that don't read Claude Code
// subagent files natively. They degrade gracefully: the memory-load
// instructions are dropped (no memory dynamics) and the body is condensed.
//
// Phase 1 prints to stdout so the user can redirect into the destination file
// (e.g. `shipmates render security --target cursor > .cursor/rules/security.mdc`).
func Render(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "render",
		Usage:     "render a persona for a thin target (agents-md|cursor|windsurf)",
		ArgsUsage: "<persona>",
		Flags:     []cli.Flag{&cli.StringFlag{Name: "target", Required: true}},
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

			var out string
			switch c.String("target") {
			case "agents-md":
				out = renderAgentsMD(fm, body)
			case "cursor":
				out = renderCursor(fm, body)
			case "windsurf":
				out = renderWindsurf(fm, body)
			default:
				return fmt.Errorf("unknown target %q (want: agents-md|cursor|windsurf)", c.String("target"))
			}

			fmt.Print(out)
			return nil
		},
	}
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
// fields and the one block-sequence (domainGlob) that thin targets use. It
// ignores nested maps (permissions) and unrecognized fields — exactly the
// "drop memory dynamics" behavior we want.
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
			continue // skip nested (indented) keys
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
