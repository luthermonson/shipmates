package commands

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/luthermonson/shipmates/internal/bridge"
	"github.com/luthermonson/shipmates/internal/client"
	"github.com/urfave/cli/v3"
)

// Bridge opens "the bridge": a machine-wide terminal UI over the coordination
// server's PTY endpoints. Run it when you are SSH'd into the VM or pod hosting a
// ship — it is the operator surface the fleet web UI provides, minus the browser.
//
// Deliberately takes no persona argument. The point is every mate on the machine
// in one place, with approvals visible on background tabs.
func Bridge() *cli.Command {
	return &cli.Command{
		Name:  "bridge",
		Usage: "tabbed terminal UI for every mate live on this machine",
		Description: "Tabs across every live persona, each with its own live terminal fed from\n" +
			"the ship's PTY host.\n" +
			"\n" +
			"Two modes, and there is no prefix key.\n" +
			"\n" +
			"  NAVIGATE (the mode it opens in) — 1-9 selects a mate, arrows/h/l/tab\n" +
			"  move between them, enter starts typing at the selected one, a decides\n" +
			"  pending approvals, ? shows help, q quits.\n" +
			"\n" +
			"  TYPE — every key goes to the mate untouched, ^b and ^c included. esc\n" +
			"  goes back to NAVIGATE and alt+1-9 switches mates without leaving.\n" +
			"\n" +
			"The pane border tells you which mode you are in: light means the bridge\n" +
			"has your keys, heavy means the mate does. Selecting a mate connects it\n" +
			"and claims its keyboard for you; there is nothing to attach by hand.",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "persona",
				Aliases: []string{"p"},
				Usage:   "focus this persona's tab at startup",
			},
			&cli.BoolFlag{
				Name:  "start",
				Usage: "start the coordination server if it isn't running (it will have no live mates yet)",
			},
			&cli.BoolFlag{
				Name:  "no-bell",
				Usage: "don't ring the terminal bell when a new approval arrives",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if !client.Healthy() {
				if !c.Bool("start") {
					return errors.New("no shipmates server running on this machine — " +
						"put a mate to work first (shipmates tell/ask/open), or pass --start")
				}
				if err := client.EnsureRunning(); err != nil {
					return err
				}
			}
			base, err := client.BaseURL()
			if err != nil {
				return fmt.Errorf("locate coordination server: %w", err)
			}

			tok, err := client.Token()
			if err != nil {
				return fmt.Errorf("read coordination server token: %w", err)
			}

			opts := bridge.Options{
				Base:  base,
				Token: tok,
				Focus: c.String("persona"),
			}
			if !c.Bool("no-bell") {
				// Stderr, not the renderer's stdout: a bare BEL there rings the
				// terminal without touching the alt-screen frame.
				opts.Bell = os.Stderr
			}
			return bridge.Run(ctx, opts)
		},
	}
}
