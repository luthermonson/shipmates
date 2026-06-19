package commands

import (
	"context"
	"errors"

	"github.com/urfave/cli/v3"
)

// errNotImplemented marks commands whose behavior is specified in
// docs/architecture.md but not yet built.
var errNotImplemented = errors.New("not implemented yet")

func notImplemented(ctx context.Context, c *cli.Command) error {
	return errNotImplemented
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
