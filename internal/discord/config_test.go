package discord

import (
	"strings"
	"testing"
)

// setDiscordEnv sets (or, for an empty value, clears) every var Preflight reads,
// scoped to the test via t.Setenv so cases don't leak into each other.
func setDiscordEnv(t *testing.T, token, channel, persona, allowed string) {
	t.Helper()
	t.Setenv(EnvBotToken, token)
	t.Setenv(EnvChannel, channel)
	t.Setenv(EnvPersona, persona)
	t.Setenv(EnvAllowedUsers, allowed)
}

func TestConfigFromEnv_ParsesAllFields(t *testing.T) {
	setDiscordEnv(t, "tok-should-not-appear", "chan123", "captain", "111, 222 ,333")

	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("ConfigFromEnv() error = %v, want nil", err)
	}
	if cfg.Channel != "chan123" {
		t.Errorf("Channel = %q, want %q", cfg.Channel, "chan123")
	}
	if cfg.Persona != "captain" {
		t.Errorf("Persona = %q, want %q", cfg.Persona, "captain")
	}
	for _, id := range []string{"111", "222", "333"} {
		if !cfg.Allowed.Allows(id) {
			t.Errorf("allowlist should contain %q", id)
		}
	}
	if cfg.PollInterval != DefaultPollInterval {
		t.Errorf("PollInterval = %v, want default %v", cfg.PollInterval, DefaultPollInterval)
	}
}

func TestConfigFromEnv_TrimsWhitespace(t *testing.T) {
	setDiscordEnv(t, "tok", "  chan123  ", "  captain  ", "111")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Channel != "chan123" || cfg.Persona != "captain" {
		t.Errorf("whitespace not trimmed: channel=%q persona=%q", cfg.Channel, cfg.Persona)
	}
}

func TestConfigFromEnv_MissingRequired(t *testing.T) {
	cases := []struct {
		name                             string
		token, channel, persona, allowed string
		wantVar                          string
	}{
		{"missing channel", "tok", "", "captain", "111", EnvChannel},
		{"missing persona", "tok", "chan123", "", "111", EnvPersona},
		{"blank channel", "tok", "   ", "captain", "111", EnvChannel},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDiscordEnv(t, tc.token, tc.channel, tc.persona, tc.allowed)
			_, err := ConfigFromEnv()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("error %q should name the missing var %q", err, tc.wantVar)
			}
		})
	}
}

func TestPreflight_OK(t *testing.T) {
	setDiscordEnv(t, "a-secret-token", "chan123", "captain", "111")
	cfg, err := Preflight()
	if err != nil {
		t.Fatalf("Preflight() error = %v, want nil", err)
	}
	if cfg.Channel != "chan123" || cfg.Persona != "captain" || !cfg.Allowed.Allows("111") {
		t.Errorf("Preflight returned unexpected config: %+v", cfg)
	}
}

func TestPreflight_MissingVarNamedAndTokenNeverEchoed(t *testing.T) {
	const secret = "super-secret-bot-token-value"
	cases := []struct {
		name                             string
		token, channel, persona, allowed string
		wantVar                          string
	}{
		{"missing token", "", "chan123", "captain", "111", EnvBotToken},
		{"blank token", "   ", "chan123", "captain", "111", EnvBotToken},
		{"missing channel", secret, "", "captain", "111", EnvChannel},
		{"missing persona", secret, "chan123", "", "111", EnvPersona},
		{"empty allowlist", secret, "chan123", "captain", "", EnvAllowedUsers},
		{"whitespace allowlist", secret, "chan123", "captain", " , , ", EnvAllowedUsers},
		{"invalid persona", secret, "chan123", "Bad/Name", "111", EnvPersona},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setDiscordEnv(t, tc.token, tc.channel, tc.persona, tc.allowed)
			_, err := Preflight()
			if err == nil {
				t.Fatal("expected an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantVar) {
				t.Errorf("error %q should name the offending var %q", err, tc.wantVar)
			}
			// The token's VALUE must never leak into an error, whatever failed.
			if strings.Contains(err.Error(), secret) {
				t.Errorf("error message leaked the bot token value: %q", err)
			}
		})
	}
}

func TestPreflight_EmptyAllowlistIsRejectedAtCLI(t *testing.T) {
	// The library keeps empty-allowlist as valid fail-closed policy (Allows
	// returns false for all), but the command-level Preflight refuses it so an
	// operator doesn't start a bot that can never accept a command.
	setDiscordEnv(t, "tok", "chan123", "captain", "")
	if _, err := Preflight(); err == nil {
		t.Fatal("Preflight must reject an empty allowlist")
	}
	// The library primitive still treats empty as fail-closed, not an error.
	if ParseAllowlist("").Allows("111") {
		t.Error("empty allowlist must still reject everyone at the library level")
	}
}
