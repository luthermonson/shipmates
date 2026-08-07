package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/luthermonson/shipmates/internal/api"
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
			return printPending(os.Stdout, out)
		},
	}
}

func printPending(w io.Writer, body []byte) error {
	var pending []api.Pending
	if err := json.Unmarshal(body, &pending); err != nil {
		return fmt.Errorf("decode pending requests: %w", err)
	}
	if len(pending) == 0 {
		_, err := fmt.Fprintln(w, "(none)")
		return err
	}
	for _, p := range pending {
		if _, err := fmt.Fprintf(w, "%s  %s wants %s\n", p.ID, p.Persona, p.Tool); err != nil {
			return err
		}
	}
	return nil
}

// Allow approves a pending permission request by id.
//
// A --for flag turns the one-time approval into a time-box: the ship
// remembers the (persona, exact-command) pair and auto-allows it for the
// given duration without prompting again. `--for 5m`, `--for 30m`, `--for 1h`
// are the common forms; any Go time.ParseDuration string is accepted.
// Omitting the flag preserves the historical one-time-only behavior.
func Allow() *cli.Command {
	return &cli.Command{
		Name:      "allow",
		Usage:     "approve a pending crew tool request",
		ArgsUsage: "<id>",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "for",
				Usage: "auto-allow this exact command from this persona for the given duration (e.g. 5m, 30m, 1h)",
			},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return errors.New("usage: shipmates allow <id> [--for <duration>]")
			}
			if !client.Healthy() {
				return errors.New("no server running")
			}
			body := map[string]string{"behavior": "allow"}
			if raw := c.String("for"); raw != "" {
				d, err := time.ParseDuration(raw)
				if err != nil {
					return fmt.Errorf("bad --for duration %q: %w", raw, err)
				}
				if d <= 0 {
					return fmt.Errorf("--for duration must be positive, got %s", raw)
				}
				body["duration"] = d.String()
			}
			if _, err := client.Post("/resolve/"+id, body); err != nil {
				return err
			}
			if body["duration"] != "" {
				fmt.Printf("allowed %s (time-boxed for %s)\n", id, body["duration"])
			} else {
				fmt.Printf("allowed %s\n", id)
			}
			return nil
		},
	}
}

// Deny rejects a pending permission request by id.
func Deny() *cli.Command {
	return &cli.Command{
		Name:      "deny",
		Usage:     "reject a pending crew tool request",
		ArgsUsage: "<id>",
		Action: func(ctx context.Context, c *cli.Command) error {
			id := c.Args().First()
			if id == "" {
				return errors.New("usage: shipmates deny <id>")
			}
			if !client.Healthy() {
				return errors.New("no server running")
			}
			if _, err := client.Post("/resolve/"+id, map[string]string{"behavior": "deny"}); err != nil {
				return err
			}
			fmt.Printf("denied %s\n", id)
			return nil
		},
	}
}
