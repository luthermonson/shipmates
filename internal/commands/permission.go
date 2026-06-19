package commands

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/urfave/cli/v3"
)

// Pending lists crew permission requests awaiting a decision.
func Pending() *cli.Command {
	return &cli.Command{
		Name:  "pending",
		Usage: "list crew permission requests awaiting allow/deny",
		Action: func(ctx context.Context, c *cli.Command) error {
			if !client.Healthy() {
				return errors.New("no server running")
			}
			out, err := client.Get("/pending")
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(out)
			return nil
		},
	}
}

// Allow approves a pending permission request by id.
func Allow() *cli.Command {
	return resolveCmd("allow", "approve a pending crew tool request")
}

// Deny rejects a pending permission request by id.
func Deny() *cli.Command {
	return resolveCmd("deny", "reject a pending crew tool request")
}

func resolveCmd(behavior, usage string) *cli.Command {
	return &cli.Command{
		Name:      behavior,
		Usage:     usage,
		ArgsUsage: "<id>",
		Action: func(ctx context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return fmt.Errorf("usage: shipmates %s <id>", behavior)
			}
			if !client.Healthy() {
				return errors.New("no server running")
			}
			if _, err := client.Post("/resolve/"+id, map[string]string{"behavior": behavior}); err != nil {
				return err
			}
			fmt.Printf("%sed %s\n", behavior, id)
			return nil
		},
	}
}
