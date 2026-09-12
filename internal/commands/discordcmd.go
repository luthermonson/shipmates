package commands

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/luthermonson/shipmates/internal/discord"
	"github.com/urfave/cli/v3"
)

// Discord runs the EXPERIMENTAL Discord chat transport: each mate speaks as its
// own Discord bot, inbound and outbound, with an optional shared all-hands
// channel. It is a single run-until-killed process with no sibling operations,
// so it is a bare top-level command (like `bridge`), not a `discord serve`
// group (unlike `server`/`fleet`/`ship`, which each carry multiple subcommands).
//
// All configuration is operator-owned and lives outside every checkout. Bots
// are defined in ~/.shipmates/discord.yaml (multi-bot mode); if that file is
// absent, the command falls back to the single-bot environment variables
// (quickstart). A bot token is NEVER in config — the file/env names the env var
// that holds it, and the token is read at startup and never printed or logged.
func Discord() *cli.Command {
	return &cli.Command{
		Name:  "discord",
		Usage: "run the experimental Discord transport (one bot per mate, + all-hands)",
		Description: "EXPERIMENTAL. Each mate speaks as its own Discord bot: an allowlisted\n" +
			"user's message in a mate's channel becomes a tell to that persona, and\n" +
			"the persona's replies are posted back — and, if configured, mirrored to a\n" +
			"shared all-hands channel (post-only).\n" +
			"\n" +
			"Requires a running ship on this machine (the captain's coordination\n" +
			"server) — put a mate to work first with shipmates tell/ask/open.\n" +
			"\n" +
			"MULTI-BOT MODE — operator config at ~/.shipmates/discord.yaml:\n" +
			"\n" +
			"  allowedUsers: [\"<user-id>\", ...]   # who may command any mate (fail-closed)\n" +
			"  allHandsChannel: \"<channel-id>\"    # optional shared channel; omit to disable\n" +
			"  mates:\n" +
			"    architect:\n" +
			"      tokenEnv: DISCORD_TOKEN_ARCHITECT   # NAME of the env var with the token\n" +
			"      channel: \"<channel-id>\"\n" +
			"\n" +
			"The token is read from the env var named by tokenEnv; it is never in the\n" +
			"file and never logged. A mate whose token env is unset is skipped with a\n" +
			"warning; the healthy mates still start.\n" +
			"\n" +
			"SINGLE-BOT MODE — used when discord.yaml is absent, all from the env:\n" +
			"\n" +
			"  DISCORD_BOT_TOKEN                the bot token (secret; never logged)\n" +
			"  DISCORD_TRAINING_CHANNEL        the channel id to listen on and post to\n" +
			"  SHIPMATES_DISCORD_ALLOWED_USERS comma-separated user ids (fail-closed)\n" +
			"  SHIPMATES_DISCORD_PERSONA       the persona a tell is addressed to\n" +
			"\n" +
			"Runs until interrupted (Ctrl-C / SIGTERM), then shuts down cleanly.",
		Action: func(ctx context.Context, c *cli.Command) error {
			// Resolve config before opening any connection. Prefers
			// ~/.shipmates/discord.yaml; falls back to the single-bot env vars
			// when it is absent. A fatal error (broken file, empty allowlist, no
			// mates, or missing env in single-bot mode) names the problem and
			// never echoes a token value.
			configs, mode, failed, err := discord.Resolve("")
			if err != nil {
				return err
			}
			// Per-mate failures are warnings, not aborts: an operator running a
			// fleet of bots should not lose every mate because one has a missing
			// token env. Name the mate and the reason; the token value cannot
			// appear because MateError carries only the env-var name.
			for _, f := range failed {
				slog.Warn("discord: skipping mate that failed preflight", "persona", f.Persona, "err", f.Err)
			}
			if len(configs) == 0 {
				return errors.New("discord: no healthy mates to run (all failed preflight — see warnings above)")
			}
			slog.Info("discord: starting", "mode", mode, "mates", len(configs))

			// Cancel on Ctrl-C (SIGINT) and SIGTERM by cancelling the context all
			// transports run on. No existing command wires signals (main.go uses a
			// background context and the servers rely on process teardown or
			// /shutdown), so the handler is installed here, derived from the
			// passed ctx, rather than mutating the shared root context. On cancel
			// every transport returns context.Canceled and RunMany waits for all
			// of them to finish before returning.
			ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
			defer stop()

			return discord.RunMany(ctx, configs)
		},
	}
}
