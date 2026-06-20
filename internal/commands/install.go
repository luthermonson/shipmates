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
	block, err := cat.RoutingFile(conf.Routing)
	if errors.Is(err, fs.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return nil, err
	}
	var b bytes.Buffer
	b.Write(bytes.TrimRight(base, "\n"))
	fmt.Fprintf(&b, "\n\n<!-- shipmates:routing:%s -->\n", conf.Routing)
	b.Write(bytes.TrimRight(block, "\n"))
	fmt.Fprintf(&b, "\n<!-- /shipmates:routing:%s -->\n", conf.Routing)
	return b.Bytes(), nil
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

	return m.Save()
}
