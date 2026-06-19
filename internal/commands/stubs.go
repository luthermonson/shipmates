package commands

import (
	"context"
	"errors"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/urfave/cli/v3"
)

// errNotImplemented marks commands whose behavior is specified in
// docs/architecture.md but not yet built.
var errNotImplemented = errors.New("not implemented yet")

func notImplemented(ctx context.Context, c *cli.Command) error {
	return errNotImplemented
}

// Remove removes a persona's subagent file, keeping memory unless --purge.
func Remove() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "remove a persona's subagent file (keeps memory unless --purge)",
		ArgsUsage: "<persona>",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "purge", Usage: "also delete the persona's memory dir"}},
		Action:    notImplemented,
	}
}

// Update refreshes installed files from the embedded catalog with diff-on-conflict.
func Update(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "update",
		Usage:     "refresh installed files from the embedded catalog (diff on conflict)",
		ArgsUsage: "[persona]",
		Flags:     []cli.Flag{&cli.StringFlag{Name: "accept", Usage: "non-interactive conflict resolution: ours|theirs"}},
		Action:    notImplemented,
	}
}

// Render emits a thin-target version of a persona.
func Render(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "render",
		Usage:     "render a persona for a thin target (agents-md|cursor|windsurf)",
		ArgsUsage: "<persona>",
		Flags:     []cli.Flag{&cli.StringFlag{Name: "target", Required: true}},
		Action:    notImplemented,
	}
}

// Open launches a long-running interactive session for a persona.
func Open() *cli.Command {
	return &cli.Command{
		Name:      "open",
		Usage:     "launch a long-running interactive session for a persona",
		ArgsUsage: "<persona>",
		Action:    notImplemented,
	}
}

// Fanout runs parallel delegations across several personas.
func Fanout() *cli.Command {
	return &cli.Command{
		Name:      "fanout",
		Usage:     "run parallel delegations across personas",
		ArgsUsage: "<p1,p2,...> <prompt>",
		Action:    notImplemented,
	}
}
