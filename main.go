package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/commands"
	"github.com/urfave/cli/v3"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cat := catalog.New(catalogFS)

	public := commands.PublicCommands(cat)
	// The Linux runtime installer is a root-level distribution command. Keep it
	// out of the historical PublicCommands tree so its frozen product-command
	// inventory remains stable for project/persona operations.
	if runtime := commands.RuntimeInstall(); runtime != nil {
		public = append([]*cli.Command{runtime}, public...)
	}
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
		Commands: public,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := cmd.Run(ctx, os.Args); err != nil {
		slog.Error("command failed", "err", err)
		os.Exit(1)
	}
}
