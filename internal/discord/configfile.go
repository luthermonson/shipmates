package discord

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/project"
)

// The operator's Discord config lives at ~/.shipmates/discord.yaml — outside
// every repo checkout, the same trust posture as ~/.shipmates/personas.yaml and
// the openai runtime's api_key_env. It names one bot per mate and, per the
// project-wide rule, a bot token is NEVER written here: each mate names the
// environment variable (tokenEnv) that holds its token, which is read at
// startup and never logged.
//
//	allowedUsers: ["<discord-user-id>", ...]   # who may command any mate; fail-closed
//	allHandsChannel: "<channel-id>"            # optional shared channel; omit to disable
//	mates:
//	  architect:
//	    tokenEnv: DISCORD_TOKEN_ARCHITECT       # NAME of the env var holding the token
//	    channel: "<channel-id>"
//	  security:
//	    tokenEnv: DISCORD_TOKEN_SECURITY
//	    channel: "<channel-id>"

// DiscordConfigName is the operator's Discord config file under ~/.shipmates/.
const DiscordConfigName = "discord.yaml"

// ErrNoConfigFile signals that ~/.shipmates/discord.yaml does not exist (or the
// home directory is undiscoverable). It is not a failure — it is the signal to
// fall back to single-bot environment-variable mode.
var ErrNoConfigFile = errors.New("discord: no ~/.shipmates/discord.yaml")

// FileConfig is the on-disk shape of ~/.shipmates/discord.yaml.
type FileConfig struct {
	// AllowedUsers is the shared, fail-closed allowlist of Discord user ids
	// permitted to command ANY mate. Empty means nobody can command.
	AllowedUsers []string `yaml:"allowedUsers"`
	// AllHandsChannel, when set, is a shared channel every mate ALSO posts its
	// outbound replies to. Omit to disable.
	AllHandsChannel string `yaml:"allHandsChannel"`
	// Mates maps a persona name to its bot binding. The key is the persona.
	Mates map[string]FileMate `yaml:"mates"`
}

// FileMate is one mate's bot binding. TokenEnv NAMES the env var holding the
// token; the token itself is never in the file.
type FileMate struct {
	TokenEnv string `yaml:"tokenEnv"`
	Channel  string `yaml:"channel"`
}

// MateError records one mate that failed preflight, so the orchestrator can
// warn about it by name without aborting the healthy mates.
type MateError struct {
	Persona string
	Err     error
}

func (m MateError) Error() string { return fmt.Sprintf("mate %q: %v", m.Persona, m.Err) }
func (m MateError) Unwrap() error { return m.Err }

// DiscordConfigPath returns ~/.shipmates/discord.yaml. An empty home resolves
// via os.UserHomeDir; if that fails the second return is false and callers
// should treat the file as absent.
func DiscordConfigPath(home string) (string, bool) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		home = h
	}
	return filepath.Join(home, project.Dir, DiscordConfigName), true
}

// LoadFile reads and parses ~/.shipmates/discord.yaml. A missing file (or an
// undiscoverable home) returns ErrNoConfigFile so the caller can fall back to
// env mode; a present-but-broken file returns a real parse error.
func LoadFile(home string) (FileConfig, error) {
	path, ok := DiscordConfigPath(home)
	if !ok {
		return FileConfig{}, ErrNoConfigFile
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return FileConfig{}, ErrNoConfigFile
		}
		return FileConfig{}, fmt.Errorf("read %s: %w", path, err)
	}
	var fc FileConfig
	if err := yaml.Unmarshal(raw, &fc); err != nil {
		return FileConfig{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return fc, nil
}

// Mates turns a parsed FileConfig into validated per-mate Configs. It returns
// the healthy configs and a slice of per-mate failures (bad persona, empty
// channel, missing/empty token env), in deterministic persona order.
//
// A STRUCTURAL problem — an empty shared allowlist, or no mates at all — is
// returned as err instead: nothing can run correctly, so the command should
// stop rather than come up degraded. A single mate's misconfiguration is NOT
// structural; it is reported in failed so the rest can still start.
//
// The allowlist is shared across every mate and is fail-closed: an empty
// allowedUsers is refused here, exactly as an empty env allowlist is refused in
// Preflight.
func Mates(fc FileConfig) (ok []Config, failed []MateError, err error) {
	allowed := ParseAllowlistSlice(fc.AllowedUsers)
	if len(allowed) == 0 {
		return nil, nil, fmt.Errorf("discord: allowedUsers is empty; set at least one Discord user id — an empty allowlist means no one can command any mate")
	}
	if len(fc.Mates) == 0 {
		return nil, nil, fmt.Errorf("discord: no mates defined under 'mates:' in %s", DiscordConfigName)
	}

	names := make([]string, 0, len(fc.Mates))
	for n := range fc.Mates {
		names = append(names, n)
	}
	sort.Strings(names)

	allHands := strings.TrimSpace(fc.AllHandsChannel)
	for _, name := range names {
		entry := fc.Mates[name]
		cfg := Config{
			Persona:         name,
			Channel:         strings.TrimSpace(entry.Channel),
			TokenEnv:        strings.TrimSpace(entry.TokenEnv),
			Allowed:         allowed,
			AllHandsChannel: allHands,
			PollInterval:    DefaultPollInterval,
		}
		if e := cfg.preflight(); e != nil {
			failed = append(failed, MateError{Persona: name, Err: e})
			continue
		}
		ok = append(ok, cfg)
	}
	return ok, failed, nil
}

// Resolve determines what to run. It prefers ~/.shipmates/discord.yaml; if that
// file is absent it falls back to the single-bot environment-variable mode
// (the quickstart the spike shipped with). It returns the validated Configs,
// a mode string ("file"|"env") for logging, per-mate failures to warn about
// (file mode only), and a fatal error for a broken/empty config.
func Resolve(home string) (configs []Config, mode string, failed []MateError, err error) {
	fc, ferr := LoadFile(home)
	if errors.Is(ferr, ErrNoConfigFile) {
		cfg, perr := Preflight()
		if perr != nil {
			return nil, "env", nil, perr
		}
		return []Config{cfg}, "env", nil, nil
	}
	if ferr != nil {
		return nil, "file", nil, ferr
	}
	okConfigs, failedMates, merr := Mates(fc)
	if merr != nil {
		return nil, "file", nil, merr
	}
	return okConfigs, "file", failedMates, nil
}
