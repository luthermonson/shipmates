package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Remove removes a persona's subagent file, keeping memory unless --purge.
func Remove() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "remove a persona's subagent file (keeps memory unless --purge)",
		ArgsUsage: "<persona>",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "purge", Usage: "also delete the persona's memory dir"}},
		Action: func(ctx context.Context, c *cli.Command) error {
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

			codexPath := project.CodexAgentPath(name)
			if err := os.Remove(codexPath); err == nil {
				slog.Info("removed Codex agent", "persona", name, "path", codexPath)
			} else if !os.IsNotExist(err) {
				return err
			}
			delete(m.Files, codexPath)

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

			return m.Save()
		},
	}
}
