package discord

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

func TestMates_ParsesAndResolvesTokenEnv(t *testing.T) {
	// The token VALUE lives only in the environment; the file names the var.
	t.Setenv("DISCORD_TOKEN_ARCHITECT", "secret-architect-token")
	t.Setenv("DISCORD_TOKEN_SECURITY", "secret-security-token")

	fc := FileConfig{
		AllowedUsers:    []string{"111", " 222 "},
		AllHandsChannel: "all-hands-chan",
		Mates: map[string]FileMate{
			"architect": {TokenEnv: "DISCORD_TOKEN_ARCHITECT", Channel: "chan-a"},
			"security":  {TokenEnv: "DISCORD_TOKEN_SECURITY", Channel: "chan-s"},
		},
	}
	ok, failed, err := Mates(fc)
	if err != nil {
		t.Fatalf("Mates() err = %v, want nil", err)
	}
	if len(failed) != 0 {
		t.Fatalf("unexpected failures: %v", failed)
	}
	if len(ok) != 2 {
		t.Fatalf("got %d configs, want 2", len(ok))
	}
	// Deterministic (sorted) order: architect before security.
	if ok[0].Persona != "architect" || ok[1].Persona != "security" {
		t.Errorf("mates not in sorted order: %q, %q", ok[0].Persona, ok[1].Persona)
	}
	for _, cfg := range ok {
		// Config carries the env-var NAME, never the token value.
		if strings.Contains(cfg.TokenEnv, "secret-") {
			t.Errorf("Config.TokenEnv holds a token value, not a name: %q", cfg.TokenEnv)
		}
		if cfg.AllHandsChannel != "all-hands-chan" {
			t.Errorf("all-hands not propagated to %q: %q", cfg.Persona, cfg.AllHandsChannel)
		}
		// Shared fail-closed allowlist.
		if !cfg.Allowed.Allows("111") || !cfg.Allowed.Allows("222") || cfg.Allowed.Allows("999") {
			t.Errorf("shared allowlist wrong for %q", cfg.Persona)
		}
	}
}

func TestMates_MissingTokenEnvNamesVarAndNeverLeaksValue(t *testing.T) {
	const secret = "super-secret-token-value"
	t.Setenv("DISCORD_TOKEN_ARCHITECT", secret)
	// DISCORD_TOKEN_SECURITY intentionally left unset.
	t.Setenv("DISCORD_TOKEN_SECURITY", "")

	fc := FileConfig{
		AllowedUsers: []string{"111"},
		Mates: map[string]FileMate{
			"architect": {TokenEnv: "DISCORD_TOKEN_ARCHITECT", Channel: "chan-a"},
			"security":  {TokenEnv: "DISCORD_TOKEN_SECURITY", Channel: "chan-s"},
		},
	}
	ok, failed, err := Mates(fc)
	if err != nil {
		t.Fatalf("Mates() err = %v, want nil (per-mate failure is not structural)", err)
	}
	// architect healthy, security failed — the rest still come up.
	if len(ok) != 1 || ok[0].Persona != "architect" {
		t.Fatalf("expected only architect healthy, got %+v", ok)
	}
	if len(failed) != 1 || failed[0].Persona != "security" {
		t.Fatalf("expected security to fail, got %+v", failed)
	}
	msg := failed[0].Err.Error()
	if !strings.Contains(msg, "DISCORD_TOKEN_SECURITY") {
		t.Errorf("failure should name the missing env var, got %q", msg)
	}
	// Whatever failed, no token value may appear in the message.
	if strings.Contains(msg, secret) {
		t.Errorf("error leaked a token value: %q", msg)
	}
}

func TestMates_StructuralErrors(t *testing.T) {
	cases := []struct {
		name    string
		fc      FileConfig
		wantSub string
	}{
		{
			name:    "empty allowlist",
			fc:      FileConfig{Mates: map[string]FileMate{"a": {TokenEnv: "X", Channel: "c"}}},
			wantSub: "allowedUsers",
		},
		{
			name:    "whitespace-only allowlist",
			fc:      FileConfig{AllowedUsers: []string{"  ", ""}, Mates: map[string]FileMate{"a": {TokenEnv: "X", Channel: "c"}}},
			wantSub: "allowedUsers",
		},
		{
			name:    "no mates",
			fc:      FileConfig{AllowedUsers: []string{"111"}},
			wantSub: "no mates",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Mates(tc.fc)
			if err == nil {
				t.Fatal("expected a structural error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q should mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestMates_AllHandsOptional(t *testing.T) {
	t.Setenv("DISCORD_TOKEN_A", "tok")
	fc := FileConfig{
		AllowedUsers: []string{"111"},
		// allHandsChannel omitted.
		Mates: map[string]FileMate{"a": {TokenEnv: "DISCORD_TOKEN_A", Channel: "chan-a"}},
	}
	ok, _, err := Mates(fc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok[0].AllHandsChannel != "" {
		t.Errorf("all-hands should be empty when omitted, got %q", ok[0].AllHandsChannel)
	}
}

func TestMates_InvalidPersonaFails(t *testing.T) {
	t.Setenv("DISCORD_TOKEN_BAD", "tok")
	fc := FileConfig{
		AllowedUsers: []string{"111"},
		Mates:        map[string]FileMate{"Bad/Name": {TokenEnv: "DISCORD_TOKEN_BAD", Channel: "c"}},
	}
	ok, failed, err := Mates(fc)
	if err != nil {
		t.Fatalf("unexpected structural error: %v", err)
	}
	if len(ok) != 0 || len(failed) != 1 {
		t.Fatalf("expected the bad persona to fail preflight, ok=%v failed=%v", ok, failed)
	}
}

// writeDiscordYAML writes content to <home>/.shipmates/discord.yaml.
func writeDiscordYAML(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, project.Dir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, DiscordConfigName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadFile_MissingIsErrNoConfigFile(t *testing.T) {
	home := t.TempDir() // no discord.yaml written
	_, err := LoadFile(home)
	if !errors.Is(err, ErrNoConfigFile) {
		t.Errorf("missing file: err = %v, want ErrNoConfigFile", err)
	}
}

func TestLoadFile_ParsesYAML(t *testing.T) {
	home := t.TempDir()
	writeDiscordYAML(t, home, `
allowedUsers: ["111", "222"]
allHandsChannel: "all-hands"
mates:
  architect:
    tokenEnv: DISCORD_TOKEN_ARCHITECT
    channel: "chan-a"
`)
	fc, err := LoadFile(home)
	if err != nil {
		t.Fatalf("LoadFile err = %v", err)
	}
	if len(fc.AllowedUsers) != 2 || fc.AllHandsChannel != "all-hands" {
		t.Errorf("top-level fields wrong: %+v", fc)
	}
	m, ok := fc.Mates["architect"]
	if !ok || m.TokenEnv != "DISCORD_TOKEN_ARCHITECT" || m.Channel != "chan-a" {
		t.Errorf("mate parse wrong: %+v", fc.Mates)
	}
}

func TestResolve_FileMode(t *testing.T) {
	t.Setenv("DISCORD_TOKEN_ARCHITECT", "tok")
	home := t.TempDir()
	writeDiscordYAML(t, home, `
allowedUsers: ["111"]
mates:
  architect:
    tokenEnv: DISCORD_TOKEN_ARCHITECT
    channel: "chan-a"
`)
	configs, mode, failed, err := Resolve(home)
	if err != nil {
		t.Fatalf("Resolve err = %v", err)
	}
	if mode != "file" {
		t.Errorf("mode = %q, want file", mode)
	}
	if len(failed) != 0 {
		t.Errorf("unexpected failures: %v", failed)
	}
	if len(configs) != 1 || configs[0].Persona != "architect" {
		t.Errorf("configs wrong: %+v", configs)
	}
}

func TestResolve_FallsBackToEnvWhenNoFile(t *testing.T) {
	// No discord.yaml in this home → env single-bot mode.
	home := t.TempDir()
	t.Setenv(EnvBotToken, "tok")
	t.Setenv(EnvChannel, "chan")
	t.Setenv(EnvPersona, "captain")
	t.Setenv(EnvAllowedUsers, "111")

	configs, mode, _, err := Resolve(home)
	if err != nil {
		t.Fatalf("Resolve err = %v", err)
	}
	if mode != "env" {
		t.Errorf("mode = %q, want env", mode)
	}
	if len(configs) != 1 || configs[0].Persona != "captain" || configs[0].TokenEnv != EnvBotToken {
		t.Errorf("env-mode config wrong: %+v", configs)
	}
}

func TestResolve_EnvFallbackStillFailClosed(t *testing.T) {
	home := t.TempDir() // no file → env mode
	t.Setenv(EnvBotToken, "tok")
	t.Setenv(EnvChannel, "chan")
	t.Setenv(EnvPersona, "captain")
	t.Setenv(EnvAllowedUsers, "") // empty allowlist

	if _, _, _, err := Resolve(home); err == nil {
		t.Fatal("empty env allowlist must be refused in fallback mode too")
	}
}
