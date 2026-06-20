package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"log/slog"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Routing manages the project routing block on persona files — including custom
// personas that weren't vendored from the catalog (which `add`/`update` can't
// reach).
func Routing(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "routing",
		Usage: "manage the project routing block on persona files",
		Commands: []*cli.Command{
			{
				Name:      "apply",
				Usage:     "compose the active routing block into persona file(s)",
				ArgsUsage: "<persona-file>...",
				Flags: []cli.Flag{
					&cli.BoolFlag{Name: "all", Usage: "apply to every .claude/agents/*.md"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					conf, err := project.LoadConfig()
					if err != nil {
						return err
					}
					if conf.Routing == "" {
						return errors.New("no routing declared in shipmates.yaml (set e.g. `routing: github`)")
					}

					files := c.Args().Slice()
					if c.Bool("all") {
						m, err := filepath.Glob(filepath.Join(project.AgentsDir, "*.md"))
						if err != nil {
							return err
						}
						files = m
					}
					if len(files) == 0 {
						return errors.New("usage: shipmates routing apply <persona-file>... (or --all)")
					}

					for _, f := range files {
						content, err := os.ReadFile(f)
						if err != nil {
							return err
						}
						out, err := applyRouting(cat, content)
						if err != nil {
							return err
						}
						if err := os.WriteFile(f, out, 0o644); err != nil {
							return err
						}
						slog.Info("applied routing block", "file", f, "routing", conf.Routing)
					}
					return nil
				},
			},
		},
	}
}
