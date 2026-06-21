package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/commands"
	"github.com/urfave/cli/v3"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cat := catalog.New(catalogFS)

	cmd := &cli.Command{
		Name:    "shipmates",
		Usage:   "subagents that remember — assemble AI personas with persistent project memory",
		Version: version,
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "verbose", Usage: "enable debug logging"},
		},
		Before: func(ctx context.Context, c *cli.Command) (context.Context, error) {
			level := slog.LevelInfo
			if c.Bool("verbose") {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
			return ctx, nil
		},
		Commands: []*cli.Command{
			commands.Init(cat),
			commands.Add(cat),
			commands.List(cat),
			commands.Remove(),
			commands.Update(cat),
			commands.Render(cat),
			commands.Routing(cat),
			commands.Open(),
			commands.Ask(),
			commands.Tell(),
			commands.Feed(),
			commands.Pending(),
			commands.Allow(),
			commands.Deny(),
			commands.Fanout(),
			commands.Drain(cat),
			commands.DrainMany(cat),
			commands.Autonomous(cat),
			commands.Bridge(),
			commands.Server(),
		},
	}

	if err := cmd.Run(context.Background(), os.Args); err != nil {
		slog.Error("command failed", "err", err)
		os.Exit(1)
	}
}
