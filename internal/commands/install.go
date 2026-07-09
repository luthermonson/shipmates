package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// defaultConfig renders the starter shipmates.yaml, baking in the session
// prefix (the repo name) at init time. Leave sessionPrefix empty for no prefix.
func defaultConfig(sessionPrefix string) string {
	return fmt.Sprintf(`# shipmates.yaml — crew configuration for this project
# Personas live as Claude Code subagent files in .claude/agents/.
# Per-persona overrides (permission mode, remoteControl, model/effort) go here.

# Prefix for per-persona session names (--name / --resume handles), written as
# this repo's name at init. Leave it empty (sessionPrefix: "") for no prefix, or
# change it to disambiguate two checkouts of the same repo / same-named projects.
sessionPrefix: %s

# Per-persona overrides. Uncomment the crew key and add child keys to override
# permission mode, remoteControl, model/effort, etc. Keeping it fully commented
# means there's no active empty crew key — so appending your own crew block
# won't create a duplicate top-level key (which yaml.v3 rejects).
# crew:
#   security:
#     permissions: { mode: ask }
#   backend:
#     dangerouslySkipPermissions: true
#   tester:
#     model: claude-haiku-4-5-20251001   # run this persona on a cheaper/faster model
#   architect:
#     effort: high                       # low|medium|high|xhigh|max

# Routing substrate. Set to "github" to append GitHub issues/PRs routing
# conventions (claim-by-label, worktree-per-issue, Closes #n, verdict merge
# gate, cleanup ceremony) to every crew persona at install/update time. Leave
# empty to stay routing-agnostic (default). Run "shipmates update" after
# changing this to (re)compose installed personas.
# routing: github
# routingOptions:        # both default true (private-fleet conventions)
#   bylines: false       # "<persona> here" intros on gh comments — off for open source
#   labels: false        # persona-name labels as a work queue — off for open source
# routingOnBoot: true    # default false. true => personas get a one-line "run
#                        # /sync-routing on boot" instruction instead of the full
#                        # block embedded — keeps persona files tiny, one source
#                        # of truth. Requires the /sync-routing command (installed).

# Set true to commit per-persona memory (shared team knowledge) instead of
# keeping it gitignored (per-developer learnings).
sharedMemory: false
`, sessionPrefix)
}

// Init scaffolds shipmates into the current project.
func Init(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "scaffold shipmates into the current project",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "crew", Usage: "comma-separated personas to add immediately"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			for _, d := range []string{
				project.Dir,
				filepath.Join(project.Dir, project.MemoryDirName),
				filepath.Join(project.Dir, project.SessionsDirName),
				project.AgentsDir,
			} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}

			if _, err := os.Stat(project.ConfigName); errors.Is(err, fs.ErrNotExist) {
				if err := os.WriteFile(project.ConfigName, []byte(defaultConfig(project.RepoName())), 0o644); err != nil {
					return err
				}
				slog.Info("wrote", "file", project.ConfigName)
			}

			m, err := project.LoadManifest()
			if err != nil {
				return err
			}
			if err := installCommands(cat, m); err != nil {
				return err
			}
			if err := ensureAttachGitignore(); err != nil {
				// Non-fatal: the inbox is regenerable and a missing
				// gitignore only means the operator has to add it by
				// hand. Warn and keep going.
				slog.Warn("could not update .gitignore for attach inbox", "err", err)
			}
			if err := m.Save(); err != nil {
				return err
			}
			slog.Info("initialized shipmates project")

			if crew := c.String("crew"); crew != "" {
				for _, name := range strings.Split(crew, ",") {
					name = strings.TrimSpace(name)
					if name == "" {
						continue
					}
					if err := addPersona(cat, name); err != nil {
						return fmt.Errorf("add %s: %w", name, err)
					}
				}
			}
			return nil
		},
	}
}

// Add vendors a persona into .claude/agents and seeds its memory.
func Add(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "vendor a persona into .claude/agents and seed its memory",
		ArgsUsage: "<persona>",
		Action: func(ctx context.Context, c *cli.Command) error {
			name := c.Args().First()
			if name == "" {
				return errors.New("usage: shipmates add <persona>")
			}
			return addPersona(cat, name)
		},
	}
}

// List shows catalog personas and which are installed.
func List(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list catalog personas and which are installed",
		Action: func(ctx context.Context, c *cli.Command) error {
			avail, err := cat.Personas()
			if err != nil {
				return err
			}
			for _, name := range avail {
				status := ""
				if _, err := os.Stat(project.AgentPath(name)); err == nil {
					status = "installed"
				}
				fmt.Printf("%-14s %s\n", name, status)
			}
			return nil
		},
	}
}

// composeAgent returns the persona's agent file with the project's routing
// block appended (wrapped in markers) when shipmates.yaml declares a routing
// layer. Composition is deterministic so `update` can recompose and diff
// against what was installed. With no routing declared (or an unknown routing
// name) the base file is returned unchanged.
func composeAgent(cat *catalog.Catalog, base []byte) ([]byte, error) {
	conf, err := project.LoadConfig()
	if err != nil {
		return nil, err
	}
	if conf.Routing == "" {
		return base, nil
	}

	var block []byte
	if conf.RoutingOnBoot {
		// Slash-command-on-boot: keep persona files tiny; the worker loads the
		// rules at session start via the /sync-routing command.
		block = []byte("On session start, immediately run `/sync-routing` to load this project's routing conventions, then proceed.")
	} else {
		b, err := renderRoutingBlock(cat)
		if err != nil {
			return nil, err
		}
		if b == nil { // unknown routing name — leave the base unchanged
			return base, nil
		}
		block = b
	}

	var b bytes.Buffer
	b.Write(bytes.TrimRight(base, "\n"))
	fmt.Fprintf(&b, "\n\n<!-- shipmates:routing:%s -->\n", conf.Routing)
	b.Write(bytes.TrimRight(block, "\n"))
	fmt.Fprintf(&b, "\n<!-- /shipmates:routing:%s -->\n", conf.Routing)
	return b.Bytes(), nil
}

// renderRoutingBlock renders the active routing template (per shipmates.yaml's
// routing: + routingOptions:). Returns nil if no routing is configured or the
// named template doesn't exist. Single source of truth for the block, shared by
// composeAgent (full mode) and `shipmates routing show` / the /sync-routing
// command.
func renderRoutingBlock(cat *catalog.Catalog) ([]byte, error) {
	conf, err := project.LoadConfig()
	if err != nil {
		return nil, err
	}
	if conf.Routing == "" {
		return nil, nil
	}
	raw, err := cat.RoutingFile(conf.Routing)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("routing").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse routing template %s: %w", conf.Routing, err)
	}
	bylines, labels := conf.RoutingOptions.Resolved()
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, struct{ Bylines, Labels, Beads bool }{bylines, labels, beadsWorkspace()}); err != nil {
		return nil, fmt.Errorf("render routing template %s: %w", conf.Routing, err)
	}
	return collapseBlankLines(rendered.Bytes()), nil
}

// attachInboxIgnorePattern is the path shipmates adds to .gitignore so a
// binary attach doesn't accidentally get committed. Kept as a package-level
// constant so both the installer and the eventual `shipmates update` share
// the exact string.
const attachInboxIgnorePattern = ".shipmates/inbox/"

// ensureAttachGitignore makes sure the project's root .gitignore ignores the
// attach inbox. If .gitignore doesn't exist yet we create one containing just
// the inbox pattern; if it exists we append the pattern only when it isn't
// already present, preserving whatever the user has above.
func ensureAttachGitignore() error {
	const path = ".gitignore"
	existing, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Fresh file — write a minimal shipmates-managed header plus the
		// pattern. Leaving the file otherwise empty is polite: the user's
		// own ignores will land underneath without conflict.
		content := "# shipmates: keep inbound binary attachments out of git\n" +
			attachInboxIgnorePattern + "\n"
		return os.WriteFile(path, []byte(content), 0o644)
	}
	if err != nil {
		return err
	}
	if gitignoreContainsPattern(existing, attachInboxIgnorePattern) {
		return nil
	}
	// Append with a leading newline only when the existing file doesn't
	// already end with one — avoids gluing the pattern onto the last user
	// entry.
	buf := bytes.TrimRight(existing, "\r\n")
	buf = append(buf, '\n', '\n')
	buf = append(buf, "# shipmates: keep inbound binary attachments out of git\n"...)
	buf = append(buf, attachInboxIgnorePattern...)
	buf = append(buf, '\n')
	return os.WriteFile(path, buf, 0o644)
}

// gitignoreContainsPattern reports whether a .gitignore body already includes
// the given pattern on a line of its own (ignoring surrounding whitespace and
// comment lines). Cheap linear scan — the file is always tiny.
func gitignoreContainsPattern(body []byte, pattern string) bool {
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == pattern {
			return true
		}
	}
	return false
}

// beadsWorkspace reports whether the project is beads-enabled (.beads/ in the
// project root). Routing templates use it to include the gh ↔ beads seam
// conventions only where a graph actually exists. Re-run `shipmates update`
// (or `routing apply`) after `bd init` to recompose personas with the section.
func beadsWorkspace() bool {
	st, err := os.Stat(".beads")
	return err == nil && st.IsDir()
}

// collapseBlankLines squeezes runs of 3+ newlines down to 2, cleaning up the
// gaps that template conditionals leave behind.
func collapseBlankLines(b []byte) []byte {
	s := string(b)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return []byte(strings.TrimSpace(s) + "\n")
}

// installCommands vendors the catalog's slash commands into .claude/commands/,
// recording each in the manifest. Existing command files that differ from what
// shipmates installed are left untouched (user edits are preserved).
func installCommands(cat *catalog.Catalog, m *project.Manifest) error {
	names, err := cat.Commands()
	if err != nil {
		return err
	}
	for _, name := range names {
		b, err := cat.CommandFile(name)
		if err != nil {
			return err
		}
		dst := project.CommandPath(name)
		if existing, err := os.ReadFile(dst); err == nil && project.SHA(existing) != m.Files[dst] {
			continue // user-edited or pre-existing — don't clobber
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			return err
		}
		m.Files[dst] = project.SHA(b)
		slog.Info("installed command", "command", name, "path", dst)
	}
	return nil
}

// applyRouting upserts the active routing block into an arbitrary persona file
// (custom or catalog): it strips any existing shipmates routing block, then
// re-appends the current one. Idempotent — re-run to re-sync after the upstream
// routing template changes.
func applyRouting(cat *catalog.Catalog, content []byte) ([]byte, error) {
	return composeAgent(cat, stripRoutingBlock(content))
}

// stripRoutingBlock removes a previously-composed <!-- shipmates:routing:* -->
// … <!-- /shipmates:routing:* --> block (and its trailing newline) if present,
// leaving everything else untouched.
func stripRoutingBlock(b []byte) []byte {
	s := string(b)
	start := strings.Index(s, "<!-- shipmates:routing:")
	endMark := "<!-- /shipmates:routing:"
	end := strings.Index(s, endMark)
	if start < 0 || end < 0 || end < start {
		return b
	}
	before := strings.TrimRight(s[:start], "\n")
	after := ""
	if nl := strings.IndexByte(s[end:], '\n'); nl >= 0 {
		after = s[end+nl+1:]
	}
	out := before
	if strings.TrimSpace(after) != "" {
		out = before + "\n\n" + strings.TrimLeft(after, "\n")
	}
	return []byte(strings.TrimRight(out, "\n") + "\n")
}

// addPersona is the shared install routine used by `add` and `init --crew`.
func addPersona(cat *catalog.Catalog, name string) error {
	if !cat.Has(name) {
		return fmt.Errorf("unknown persona %q", name)
	}

	base, err := cat.AgentFile(name)
	if err != nil {
		return fmt.Errorf("read agent file: %w", err)
	}
	agent, err := composeAgent(cat, base)
	if err != nil {
		return err
	}

	m, err := project.LoadManifest()
	if err != nil {
		return err
	}

	dst := project.AgentPath(name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, agent, 0o644); err != nil {
		return err
	}
	m.Files[dst] = project.SHA(agent)
	slog.Info("installed persona", "persona", name, "path", dst)

	// Seed memory — but never overwrite existing memory (it's sacred).
	memDir := project.MemoryDir(name)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return err
	}
	seeds, err := cat.MemorySeeds(name)
	if err != nil {
		return err
	}
	for fname, content := range seeds {
		mp := filepath.Join(memDir, fname)
		if _, err := os.Stat(mp); err == nil {
			slog.Debug("memory exists, leaving untouched", "file", mp)
			continue
		}
		if err := os.WriteFile(mp, content, 0o644); err != nil {
			return err
		}
		m.Files[mp] = project.SHA(content)
		slog.Info("seeded memory", "file", mp)
	}

	// Vendor the persona's policy overlay (permissions.allow/ask/deny) into
	// .shipmates/policies/<persona>.yaml. Same "don't clobber user edits"
	// discipline as installCommands: if the file already exists and its
	// content diverges from what shipmates last installed, leave it alone.
	if pol, err := cat.PolicyFile(name); err == nil {
		polPath := project.PolicyPath(name)
		if err := os.MkdirAll(filepath.Dir(polPath), 0o755); err != nil {
			return err
		}
		if existing, err := os.ReadFile(polPath); err == nil && project.SHA(existing) != m.Files[polPath] {
			slog.Debug("policy exists and was user-edited, leaving untouched", "file", polPath)
		} else {
			if err := os.WriteFile(polPath, pol, 0o644); err != nil {
				return err
			}
			m.Files[polPath] = project.SHA(pol)
			slog.Info("installed persona policy", "persona", name, "path", polPath)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("read policy for %s: %w", name, err)
	}

	return m.Save()
}
