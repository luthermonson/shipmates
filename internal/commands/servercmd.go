package commands

import (
	"context"
	"errors"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/server"
	"github.com/urfave/cli/v3"
)

// Server manages the transient coordination server (usually auto-spawned by
// `tell`, but exposed for manual control and the captain's SessionEnd hook).
func Server() *cli.Command {
	return &cli.Command{
		Name:  "server",
		Usage: "manage the transient captain-spawned coordination server",
		Commands: []*cli.Command{
			{
				Name:  "serve",
				Usage: "run the coordination server (usually auto-spawned)",
				Action: func(ctx context.Context, c *cli.Command) error {
					return server.New().Run(ctx)
				},
			},
			{
				Name:  "stop",
				Usage: "gracefully shut down the running server",
				Action: func(ctx context.Context, c *cli.Command) error {
					if !client.Healthy() {
						return errors.New("no server running")
					}
					_, err := client.Post("/shutdown", nil)
					return err
				},
			},
		},
	}
}
