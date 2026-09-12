package commands

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/luthermonson/shipmates/internal/discord"
	"github.com/urfave/cli/v3"
)

// Discord runs the EXPERIMENTAL Discord chat transport: one mate <-> one Discord
// bot <-> one channel, inbound and outbound. It is a single run-until-killed
// process with no sibling operations, so it is a bare top-level command
// (like `bridge`), not a `discord serve` group (unlike `server`/`fleet`/`ship`,
// which each carry multiple subcommands).
//
// All configuration is operator-owned and read from the environment; nothing
// lives in the checkout. The bot token is a secret — it is read only from
// $DISCORD_BOT_TOKEN, and is never printed, logged, or echoed in an error.
func Discord() *cli.Command {
	return &cli.Command{
		Name:  "discord",
		Usage: "run the experimental Discord transport bridging one channel to one mate",
		Description: "EXPERIMENTAL. Bridges a single Discord text channel to a single mate:\n" +
			"an allowlisted user's message becomes a tell to the persona, and the\n" +
			"persona's replies are posted back to the channel.\n" +
			"\n" +
			"Requires a running ship on this machine (the captain's coordination\n" +
			"server) — put a mate to work first with shipmates tell/ask/open.\n" +
			"\n" +
			"Configuration is read entirely from the environment:\n" +
			"\n" +
			"  DISCORD_BOT_TOKEN                the bot token (secret; never logged)\n" +
			"  DISCORD_TRAINING_CHANNEL        the channel id to listen on and post to\n" +
			"  SHIPMATES_DISCORD_ALLOWED_USERS comma-separated Discord user ids allowed\n" +
			"                                  to command the mate (fail-closed)\n" +
			"  SHIPMATES_DISCORD_PERSONA       the persona a tell is addressed to\n" +
			"\n" +
			"Runs until interrupted (Ctrl-C / SIGTERM), then shuts down cleanly.",
		Action: func(ctx context.Context, c *cli.Command) error {
			// Fail fast, before opening any connection: a missing or malformed
			// required variable is a clear one-line error naming the variable.
			// Preflight never reads or echoes the token's value.
			cfg, err := discord.Preflight()
			if err != nil {
				return err
			}

			// Cancel on Ctrl-C (SIGINT) and SIGTERM by cancelling the context the
			// transport runs on. No existing command wires signals (main.go uses a
			// background context and the servers rely on process teardown or
			// /shutdown), so the handler is installed here, derived from the
			// passed ctx, rather than mutating the shared root context.
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			// A signal-cancelled run returns context.Canceled from Run; that is a
			// clean shutdown, not a failure, so it is not surfaced as an error.
			if err := discord.New(cfg).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil
		},
	}
}
