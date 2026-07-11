package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/luthermonson/shipmates/internal/berth"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Remove removes a persona's subagent file, keeping memory unless --purge.
// If the persona has a berth (`.shipmates/berths/<persona>`), it's torn down
// via git-worktree — refusing when the berth is dirty (unless --force) or
// holds a nested per-issue worktree mid-flight.
func Remove() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "remove a persona's subagent file (keeps memory unless --purge)",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "purge", Usage: "also delete the persona's memory dir"},
			&cli.BoolFlag{Name: "force", Usage: "remove the berth even if it has uncommitted changes"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			// R1a: `remove` writes the manifest — refuse from a berth.
			if err := berth.RefuseIfInBerth("remove"); err != nil {
				return err
			}
			name := c.Args().First()
			if name == "" {
				return errors.New("usage: shipmates remove <persona>")
			}

			agentPath := project.AgentPath(name)
			if _, err := os.Stat(agentPath); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("persona %q is not installed (no %s)", name, agentPath)
				}
				return err
			}

			m, err := project.LoadManifest()
			if err != nil {
				return err
			}

			if err := os.Remove(agentPath); err != nil {
				return err
			}
			delete(m.Files, agentPath)
			slog.Info("removed persona", "persona", name, "path", agentPath)

			if c.Bool("purge") {
				memDir := project.MemoryDir(name)
				if err := os.RemoveAll(memDir); err != nil {
					return err
				}
				prefix := memDir + string(os.PathSeparator)
				for f := range m.Files {
					if f == memDir || strings.HasPrefix(f, prefix) {
						delete(m.Files, f)
					}
				}
				slog.Info("purged persona memory", "persona", name, "dir", memDir)
			} else {
				slog.Info("memory preserved", "persona", name, "dir", project.MemoryDir(name))
			}

			// Tear down the persona's berth if one exists. Non-destructive by
			// default: refuses when the berth is dirty or holds a nested
			// per-issue worktree (routing work in flight). --force bypasses
			// the dirty check; the nested-worktree refusal stands regardless.
			if err := berth.Remove(name, c.Bool("force")); err != nil {
				return fmt.Errorf("remove berth: %w", err)
			}

			return m.Save()
		},
	}
}
