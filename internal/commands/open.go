package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Open launches a long-running INTERACTIVE Claude Code session as a persona.
// Unlike Ask (one-shot -p), this hands the terminal to claude's TUI and blocks
// until the user exits. Session continuity mirrors Ask: create on first open,
// resume thereafter.
func Open() *cli.Command {
	return &cli.Command{
		Name:      "open",
		Usage:     "launch a long-running interactive session for a persona",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session instead of resuming (applies config changes like model/effort)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			if persona == "" {
				return errors.New("usage: shipmates open <persona>")
			}

			agentPath := project.AgentPath(persona)
			if _, err := os.Stat(agentPath); err != nil {
				if os.IsNotExist(err) {
					return fmt.Errorf("persona %q is not installed — run: shipmates add %s", persona, persona)
				}
				return err
			}

			cfg, idArgs, id, name, fp := sessionLaunch(persona, c.Bool("fresh"))
			args := append([]string{}, idArgs...) // interactive: no -p
			args = append(args, cfg.LaunchFlags(true)...)
			if cfg.RemoteControl != "" {
				args = append(args, "--remote-control", cfg.RemoteControl)
				fmt.Fprintf(os.Stderr,
					"remote control is ON for %q (session %q) — traffic routes through Anthropic-hosted relay infrastructure so the desktop/mobile app can drive it.\n",
					persona, cfg.RemoteControl)
			}

			cmd := exec.CommandContext(ctx, "claude", args...)
			cmd.Stdin = os.Stdin
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}
			return project.WriteSessionMeta(persona, name, id, fp)
		},
	}
}
