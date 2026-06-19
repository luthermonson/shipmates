package commands

import (
	"context"
	"errors"
	"fmt"
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
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			prompt := strings.TrimSpace(strings.Join(c.Args().Tail(), " "))
			if persona == "" || prompt == "" {
				return errors.New("usage: shipmates ask <persona> <prompt>")
			}

			name := project.SessionName(persona)
			marker := project.SessionMarker(persona)

			var args []string
			if _, err := os.Stat(marker); err == nil {
				args = []string{"-p", "--resume", name, "--agent", persona}
			} else {
				args = []string{"-p", "--session-id", project.NewUUID(), "--name", name, "--agent", persona}
			}
			if cfg, err := project.ResolvePersonaConfig(persona); err == nil && cfg.Model != "" {
				args = append(args, "--model", cfg.Model)
			}
			args = append(args, prompt)

			cmd := exec.CommandContext(ctx, "claude", args...)
			cmd.Stdin = strings.NewReader("") // immediate EOF — skip claude's ~3s stdin wait
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return err
			}

			if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
				return err
			}
			return os.WriteFile(marker, []byte(name), 0o644)
		},
	}
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
