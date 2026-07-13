package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/dashboard"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Open attaches the structured local dashboard to a Codex session.
func Open() *cli.Command {
	return &cli.Command{
		Name:      "open",
		Usage:     "launch a long-running interactive session for a persona",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session instead of resuming (applies config changes like model/effort)"},
			&cli.BoolFlag{Name: "plain", Usage: "avoid alternate-screen and cursor-addressed presentation"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			if persona == "" {
				return errors.New("usage: shipmates open <persona>")
			}

			if _, err := project.CanonicalPersonaAt(".", persona); err != nil {
				return err
			}
			guard, err := dashboard.NewGuard(dashboard.NewNativeTerminal(os.Stdin, os.Stdout), c.Bool("plain"))
			if err != nil {
				return err
			}
			if err := client.EnsureRunning(); err != nil {
				return err
			}
			conn, err := dashboard.Connect(ctx, dashboard.HTTPTransport{}, persona, dashboard.AttachRequest{Fresh: c.Bool("fresh")})
			if err != nil {
				return err
			}
			defer func() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				_ = conn.Close(releaseCtx)
			}()
			model, err := dashboard.NewModel(conn.Attach)
			if err != nil {
				return err
			}
			err = dashboard.Run(ctx, guard, func(runCtx context.Context) error {
				return dashboard.ActionLoop(runCtx, dashboard.NewNativeEditor(os.Stdin), dashboard.NativeSize(os.Stdout), dashboard.NativeRenderer(os.Stdout, c.Bool("plain")), model, conn)
			})
			if conn.Attach.State == "working" || conn.Attach.State == "steering" || conn.Attach.State == "interrupting" {
				fmt.Fprintf(c.ErrWriter, "work continues; reconnect with shipmates open %s, observe with shipmates feed, or cancel with shipmates interrupt\n", persona)
			}
			return err
		},
	}
}
