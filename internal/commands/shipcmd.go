package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/luthermonson/shipmates/internal/ship"
	"github.com/urfave/cli/v3"
)

// Ship manages the per-host supervisor: one daemon that keeps a lead server
// alive in every project dir listed in ~/.shipmates/ship.yaml.
func Ship() *cli.Command {
	return &cli.Command{
		Name:  "ship",
		Usage: "supervise this host's leads (one daemon per machine)",
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "run the supervisor: a lead server per configured project, restart on crash",
				Flags: []cli.Flag{
					&cli.StringFlag{Name: "config", Usage: "ship.yaml path (default ~/.shipmates/ship.yaml)"},
					&cli.StringFlag{Name: "log-file", Usage: "append supervisor logs to this file instead of stderr"},
				},
				Action: func(ctx context.Context, c *cli.Command) error {
					if lf := c.String("log-file"); lf != "" {
						f, err := os.OpenFile(lf, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
						if err != nil {
							return err
						}
						defer f.Close()
						slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
					}
					path := c.String("config")
					if path == "" {
						p, err := ship.ConfigPath()
						if err != nil {
							return err
						}
						path = p
					}
					conf, err := ship.LoadConfig(path)
					if err != nil {
						return fmt.Errorf("load %s: %w (list project dirs under a `projects:` key)", path, err)
					}
					return ship.Run(ctx, conf)
				},
			},
			{
				Name:  "install",
				Usage: "run the supervisor at logon (Windows: Scheduled Task; macOS: launchd user agent)",
				Action: func(ctx context.Context, c *cli.Command) error {
					path, err := ship.ConfigPath()
					if err != nil {
						return err
					}
					if _, err := ship.LoadConfig(path); err != nil {
						return fmt.Errorf("refusing to install with a broken config: %w", err)
					}
					if err := ship.Install(); err != nil {
						return err
					}
					fmt.Println("ship supervisor installed and started — logs in ~/.shipmates/ship.log")
					return nil
				},
			},
			{
				Name:  "uninstall",
				Usage: "stop the supervisor and remove its logon registration",
				Action: func(ctx context.Context, c *cli.Command) error {
					if err := ship.Uninstall(); err != nil {
						return err
					}
					fmt.Println("ship supervisor removed")
					return nil
				},
			},
		},
	}
}
