// Package discord is an EXPERIMENTAL spike of a Discord chat transport for
// shipmates: one mate <-> one Discord bot <-> one channel, inbound and
// outbound. It mirrors the voice-conversation loop in internal/fleet
// (read GET /events, drive POST /tell/{persona}) but binds it to a Discord
// text channel instead of the /api/conversation endpoint.
//
// It reuses the existing tell/events seam through internal/client, which reads
// the per-run captain bearer token from .shipmates/sessions/server.token and
// the port from server.port, and sends Authorization: Bearer on every request.
// This package adds no new plumbing to that seam — it only bridges Discord
// messages into a tell and captain events back out to the channel.
//
// SECURITY POSTURE (see docs/discord.md for the full model):
//   - The bot token is a secret. It is read ONLY from the environment, never
//     from the checkout, and is never logged or placed in an error message.
//   - Inbound Discord text is hostile input: it becomes the *content* of a
//     tell and nothing more. It is length-bounded and cannot smuggle a command.
//   - The allowlist is fail-closed: an empty allowlist commands nobody.
//   - Outbound mate text has Discord mentions neutralized and terminal escape
//     sequences scrubbed before it is posted, with AllowedMentions set to none
//     as a second layer.
package discord

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/personaname"
)

// Environment variable names. Secrets and operator-owned configuration live in
// the environment, mirroring the openai runtime's api_key_env posture: a key is
// never read from config-in-repo, only named by an env var.
const (
	// EnvBotToken names the env var holding the Discord bot token. The token
	// itself never appears in config or logs. This var already exists in the
	// operator's environment.
	EnvBotToken = "DISCORD_BOT_TOKEN"
	// EnvChannel names the env var holding the single channel id this transport
	// listens on and posts to. Already present in the operator's environment.
	EnvChannel = "DISCORD_TRAINING_CHANNEL"
	// EnvAllowedUsers names the env var holding a comma-separated allowlist of
	// Discord user ids permitted to command the mate. Fail-closed: unset or
	// empty means nobody can command.
	EnvAllowedUsers = "SHIPMATES_DISCORD_ALLOWED_USERS"
	// EnvPersona names the env var holding the persona a tell is addressed to.
	EnvPersona = "SHIPMATES_DISCORD_PERSONA"
)

// Bounds. None may be disabled by setting them to zero; an unbounded value from
// an untrusted source is how a spike turns into an incident.
const (
	// MaxInboundRunes caps the content of a single inbound tell. Discord's own
	// message cap is 2000 characters; we bound below that so a maximally long
	// message still becomes bounded tell content.
	MaxInboundRunes = 1800
	// MaxOutboundCells caps a single outbound post. Discord rejects messages
	// over 2000 characters; we leave headroom for the ellipsis and any prefix.
	MaxOutboundCells = 1900
	// DefaultPollInterval is how often the outbound loop polls GET /events.
	DefaultPollInterval = 1500 * time.Millisecond
)

// Config is the resolved, validated configuration for one Discord transport
// instance (one bot ↔ one persona ↔ one channel). The bot token VALUE is
// deliberately NOT a field: Config holds only the NAME of the env var that
// carries it (TokenEnv), so the secret never sits in a struct that could be
// logged with %+v. The value is read from the environment at connect time only.
// See readToken.
type Config struct {
	// Channel is the one channel id this transport is bound to.
	Channel string
	// Persona is the crew persona a tell is addressed to.
	Persona string
	// TokenEnv is the NAME of the environment variable holding this mate's bot
	// token — never the token itself. In single-bot env mode this is
	// EnvBotToken; in multi-bot file mode it is each mate's tokenEnv.
	TokenEnv string
	// Allowed is the fail-closed set of Discord user ids permitted to command.
	Allowed Allowlist
	// AllHandsChannel, when non-empty, is a shared channel every mate ALSO posts
	// its outbound replies to (post-only fan-out). Empty disables it.
	AllHandsChannel string
	// PollInterval is the GET /events poll cadence.
	PollInterval time.Duration
}

// preflight validates one resolved Config without connecting: the channel must
// be set, the persona must be a legal persona name (it becomes a /tell path
// segment), and the named token env var must actually hold a value. It never
// reads the token into anything but a discarded local, and its errors name the
// env var, never its value. The allowlist is validated at a higher level
// (shared across mates), not here.
func (c Config) preflight() error {
	if strings.TrimSpace(c.Channel) == "" {
		return fmt.Errorf("channel is empty")
	}
	if err := personaname.Validate(c.Persona); err != nil {
		return err
	}
	if _, err := readToken(c.TokenEnv); err != nil {
		return err
	}
	return nil
}

// ConfigFromEnv builds a Config from the operator's environment. It reads and
// validates the channel, persona, and allowlist. It deliberately does NOT read
// the bot token here — the token is fetched separately, immediately before use,
// by tokenFromEnv, so it never lands in Config.
func ConfigFromEnv() (Config, error) {
	channel := strings.TrimSpace(os.Getenv(EnvChannel))
	if channel == "" {
		return Config{}, fmt.Errorf("discord: %s is unset; set it to the target channel id", EnvChannel)
	}
	persona := strings.TrimSpace(os.Getenv(EnvPersona))
	if persona == "" {
		return Config{}, fmt.Errorf("discord: %s is unset; set it to the persona a tell should address", EnvPersona)
	}
	// Fail-closed allowlist: an unset or empty var yields an empty set, which
	// rejects everyone. ParseAllowlist never treats "empty" as "everyone".
	allowed := ParseAllowlist(os.Getenv(EnvAllowedUsers))
	return Config{
		Channel:      channel,
		Persona:      persona,
		TokenEnv:     EnvBotToken,
		Allowed:      allowed,
		PollInterval: DefaultPollInterval,
	}, nil
}

// Preflight validates that ALL required operator configuration is present and
// well-formed in the environment, WITHOUT opening a Discord connection or
// touching the ship. It is the fail-fast gate the `shipmates discord` command
// runs before it commits to a long-lived process, so a misconfiguration is a
// clear one-line error at startup rather than a confusing silent no-op.
//
// It checks, and names in its error, the exact missing/invalid variable:
//   - DISCORD_TRAINING_CHANNEL and SHIPMATES_DISCORD_PERSONA must be set
//     (via ConfigFromEnv), and the persona must be a legal persona name;
//   - DISCORD_BOT_TOKEN must be set — its VALUE is never read into the return,
//     never logged, and never echoed in the error;
//   - SHIPMATES_DISCORD_ALLOWED_USERS must list at least one id: an empty
//     allowlist is valid fail-closed policy (nobody can command) but useless as
//     a running bot, so the command refuses it with an actionable message.
func Preflight() (Config, error) {
	cfg, err := ConfigFromEnv()
	if err != nil {
		return Config{}, err
	}
	if err := personaname.Validate(cfg.Persona); err != nil {
		return Config{}, fmt.Errorf("discord: %s is invalid: %w", EnvPersona, err)
	}
	// Presence-only check on the token; the value is intentionally discarded.
	if _, err := readToken(cfg.TokenEnv); err != nil {
		return Config{}, err
	}
	if len(cfg.Allowed) == 0 {
		return Config{}, fmt.Errorf("discord: %s is unset or empty; set it to at least one Discord user id (comma-separated) — an empty allowlist means no one can command the mate", EnvAllowedUsers)
	}
	return cfg, nil
}

// readToken reads a bot token from the named environment variable at the moment
// of use. The token is never stored on Config, never logged, and a missing
// value is reported by env-var NAME only — never by echoing any value back. An
// empty envName is itself an error (a mate with no tokenEnv configured).
func readToken(envName string) (string, error) {
	envName = strings.TrimSpace(envName)
	if envName == "" {
		return "", fmt.Errorf("discord: no token env var configured (set tokenEnv, or DISCORD_BOT_TOKEN in single-bot mode)")
	}
	tok := strings.TrimSpace(os.Getenv(envName))
	if tok == "" {
		return "", fmt.Errorf("discord: %s is unset or empty; the bot token must be provided in the environment", envName)
	}
	return tok, nil
}
