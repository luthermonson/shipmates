package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/luthermonson/shipmates/internal/beads"
	"github.com/urfave/cli/v3"
)

// Beads exposes the installed bd CLI in the current Shipmates project. The
// external CLI remains the source of truth for graph schema and behavior.
func Beads() *cli.Command {
	return &cli.Command{
		Name:            "beads",
		Usage:           "manage the external Beads task graph used by plan and sail",
		ArgsUsage:       "[bd arguments...]",
		SkipFlagParsing: true,
		Action:          runBeads,
	}
}

func runBeads(ctx context.Context, c *cli.Command) error {
	// Shipmates commands are CWD-relative throughout; the project root is the
	// working directory (see internal/commands/runtime.go).
	root, err := sailProjectRoot()
	if err != nil {
		return err
	}
	command, err := beads.Available()
	if err != nil {
		return err
	}
	args := c.Args().Slice()
	if len(args) == 0 {
		if !beads.Workspace(root) {
			return errors.New("Beads is available but this project is not initialized; run: shipmates beads init")
		}
		args = []string{"status", "--json"}
	}
	client := &beads.Client{Root: root, Command: command, Timeout: beads.DefaultTimeout}
	if args[0] == "init" {
		if !containsBeadsArg(args, "--non-interactive") {
			args = append(args, "--non-interactive")
		}
		// Shipmates supplies crew context itself. Do not let bd install editor
		// agents or hooks behind the admiral's back.
		if !containsBeadsArg(args, "--skip-agents") {
			args = append(args, "--skip-agents")
		}
		if !containsBeadsArg(args, "--skip-hooks") {
			args = append(args, "--skip-hooks")
		}
	} else if !beads.Workspace(root) {
		return errors.New("Beads workspace not initialized; run: shipmates beads init")
	}
	out, err := client.Run(ctx, args...)
	if err != nil {
		return err
	}
	if strings.TrimSpace(out) != "" {
		if _, err := fmt.Fprint(c.Writer, out); err != nil {
			return err
		}
		if !strings.HasSuffix(out, "\n") {
			_, err = fmt.Fprintln(c.Writer)
			return err
		}
	}
	return nil
}

func containsBeadsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want || strings.HasPrefix(arg, want+"=") {
			return true
		}
	}
	return false
}
