package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/luthermonson/shipmates/internal/client"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Ask runs a one-shot, turn-based delegation: create the persona's session the
// first time, resume it afterward. Output streams to the terminal.
func Ask() *cli.Command {
	return &cli.Command{
		Name:      "ask",
		Usage:     "one-shot delegation to a persona (turn-based; resumes its session)",
		ArgsUsage: "<persona> <prompt>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session instead of resuming (applies config changes like model/effort)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
			if persona == "" || prompt == "" {
				return errors.New("usage: shipmates ask <persona> <prompt>")
			}
			return dispatch(ctx, persona, prompt, c.Bool("fresh"))
		},
	}
}

// dispatch runs a one-shot turn-based delegation: resolve the persona's config,
// create the session the first time / resume it after (auto-fresh on config
// drift or when fresh is requested), apply launch flags, run, and record the
// session. Shared by `ask` and `drain`. Output streams to the terminal.
func dispatch(ctx context.Context, persona, prompt string, fresh bool) error {
	return dispatchTo(ctx, persona, prompt, fresh, os.Stdout, os.Stderr)
}

// dispatchTo is dispatch with caller-supplied output writers, so parallel
// callers (drain-many) can capture each persona's output into its own buffer.
func dispatchTo(ctx context.Context, persona, prompt string, fresh bool, stdout, stderr io.Writer) error {
	cfg, idArgs, id, name, fp := sessionLaunch(persona, fresh)
	args := append([]string{"-p"}, idArgs...)
	args = append(args, cfg.LaunchFlags(true)...)
	args = append(args, prompt)

	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Stdin = strings.NewReader("") // immediate EOF — skip claude's ~3s stdin wait
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return err
	}
	return project.WriteSessionMeta(persona, name, id, fp)
}

// Tell sends a plain-string message to a live crew process via the server. The
// CLI translates the string to a stream-json user message server-side; the lead
// never touches JSON.
func Tell() *cli.Command {
	return &cli.Command{
		Name:      "tell",
		Usage:     "send a message to a live crew process while it works",
		ArgsUsage: "<persona> <message>",
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			msg := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
			if persona == "" || msg == "" {
				return errors.New("usage: shipmates tell <persona> <message>")
			}
			if err := client.EnsureRunning(); err != nil {
				return err
			}
			if _, err := client.Post("/tell/"+persona, map[string]string{"message": msg}); err != nil {
				return err
			}
			fmt.Printf("sent to %s — watch with: shipmates feed\n", persona)
			return nil
		},
	}
}

// Feed prints the server's activity feed (crew output, tells, events).
func Feed() *cli.Command {
	return &cli.Command{
		Name:  "feed",
		Usage: "print the live activity feed from the server",
		Action: func(ctx context.Context, c *cli.Command) error {
			if !client.Healthy() {
				return errors.New("no server running")
			}
			out, err := client.Get("/feed")
			if err != nil {
				return err
			}
			_, _ = os.Stdout.Write(out)
			return nil
		},
	}
}
