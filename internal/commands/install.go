package commands

import (
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

const defaultConfig = `# shipmates.yaml — crew configuration for this project
# Personas live as Claude Code subagent files in .claude/agents/.
# Per-persona overrides (permission mode, remoteControl, model/effort) go here.

# Prefix for per-persona session names (--name / --resume handles). Defaults to
# this repo's directory name when unset. Set it to disambiguate two checkouts of
# the same repo, or projects that share a directory name, on one machine.
# sessionPrefix: my-project

crew:
  # security:
  #   permissions: { mode: ask }
  # backend:
  #   dangerouslySkipPermissions: true

# Set true to commit per-persona memory (shared team knowledge) instead of
# keeping it gitignored (per-developer learnings).
sharedMemory: false
`

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
				if err := os.WriteFile(project.ConfigName, []byte(defaultConfig), 0o644); err != nil {
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

// addPersona is the shared install routine used by `add` and `init --crew`.
func addPersona(cat *catalog.Catalog, name string) error {
	if !cat.Has(name) {
		return fmt.Errorf("unknown persona %q", name)
	}

	agent, err := cat.AgentFile(name)
	if err != nil {
		return fmt.Errorf("read agent file: %w", err)
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
