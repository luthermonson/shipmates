package doctor

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckConfigFilesPresentAbsentMalformed(t *testing.T) {
	_, home := isolate(t)
	e := baseEnv(home)

	// All absent -> every operator file is an informational OK.
	for _, r := range checkConfigFiles(e) {
		if r.Status != OK {
			t.Fatalf("absent %s should be OK, got %v (%s)", r.Name, r.Status, r.Detail)
		}
	}

	// config.yaml present and clean -> OK.
	cfgPath := filepath.Join(home, ".shipmates", "config.yaml")
	writeFile(t, cfgPath, "runtime: claude\n")
	if got := findResult(t, checkConfigFiles(e), "~/.shipmates/config.yaml"); got.Status != OK {
		t.Fatalf("clean config.yaml should be OK, got %v (%s)", got.Status, got.Detail)
	}

	// config.yaml malformed -> Fail.
	writeFile(t, cfgPath, "runtime: claude\n  : : bad yaml\n")
	if got := findResult(t, checkConfigFiles(e), "~/.shipmates/config.yaml"); got.Status != Fail {
		t.Fatalf("malformed config.yaml should be Fail, got %v (%s)", got.Status, got.Detail)
	}

	// personas.yaml malformed -> Fail.
	writeFile(t, filepath.Join(home, ".shipmates", "personas.yaml"), "personas: [::bad\n")
	if got := findResult(t, checkConfigFiles(e), "~/.shipmates/personas.yaml"); got.Status != Fail {
		t.Fatalf("malformed personas.yaml should be Fail, got %v (%s)", got.Status, got.Detail)
	}
}

func TestCheckRuntimeSelectionOpenAI(t *testing.T) {
	_, home := isolate(t)

	openaiCfg := "runtime: openai\n" +
		"runtimes:\n" +
		"  openai:\n" +
		"    base_url: https://inference.internal/v1\n" +
		"    model: some-model\n" +
		"    api_key_env: OPENAI_KEY\n"
	writeFile(t, filepath.Join(home, ".shipmates", "config.yaml"), openaiCfg)

	// api_key_env set -> OK.
	e := baseEnv(home)
	e.Getenv = getenvFrom(map[string]string{"OPENAI_KEY": "secret"})
	res := checkRuntimeSelection(e)
	if got := findResult(t, res, "runtime selection"); got.Status != OK {
		t.Fatalf("selection should be OK, got %v", got.Status)
	}
	got := findResult(t, res, "openai runtime config")
	if got.Status != OK {
		t.Fatalf("openai with key set should be OK, got %v (%s)", got.Status, got.Detail)
	}
	// Never print the value.
	if containsSecret(got, "secret") {
		t.Fatalf("result leaked the secret value: %+v", got)
	}

	// api_key_env unset -> Fail (selected runtime cannot authenticate).
	e.Getenv = getenvFrom(nil)
	if got := findResult(t, checkRuntimeSelection(e), "openai runtime config"); got.Status != Fail {
		t.Fatalf("openai with key unset should be Fail, got %v (%s)", got.Status, got.Detail)
	}
}

func TestCheckRuntimeSelectionOpenAIMissingBaseURL(t *testing.T) {
	_, home := isolate(t)
	writeFile(t, filepath.Join(home, ".shipmates", "config.yaml"),
		"runtime: openai\nruntimes:\n  openai:\n    model: m\n")
	e := baseEnv(home)
	if got := findResult(t, checkRuntimeSelection(e), "openai runtime config"); got.Status != Fail {
		t.Fatalf("openai with no base_url should be Fail, got %v (%s)", got.Status, got.Detail)
	}
}

func TestCheckRuntimeSelectionDefaultClaude(t *testing.T) {
	_, home := isolate(t)
	e := baseEnv(home)
	res := checkRuntimeSelection(e)
	sel := findResult(t, res, "runtime selection")
	if sel.Status != OK {
		t.Fatalf("default selection should be OK, got %v", sel.Status)
	}
	if got := findResult(t, res, "claude runtime"); got.Status != OK {
		t.Fatalf("default claude runtime should be OK, got %v", got.Status)
	}
}

func TestCheckSecretEnvDiscord(t *testing.T) {
	_, home := isolate(t)
	discordCfg := "allowedUsers: [\"1\"]\n" +
		"mates:\n" +
		"  architect:\n" +
		"    tokenEnv: DISCORD_TOKEN_ARCHITECT\n" +
		"    channel: \"c1\"\n" +
		"  security:\n" +
		"    tokenEnv: DISCORD_TOKEN_SECURITY\n" +
		"    channel: \"c2\"\n"
	writeFile(t, filepath.Join(home, ".shipmates", "discord.yaml"), discordCfg)

	e := baseEnv(home)
	e.Getenv = getenvFrom(map[string]string{"DISCORD_TOKEN_ARCHITECT": "s3cr3tvalue"})
	res := checkSecretEnv(e)

	if got := findResult(t, res, "discord token: architect"); got.Status != OK {
		t.Fatalf("architect token set should be OK, got %v (%s)", got.Status, got.Detail)
	} else if containsSecret(got, "s3cr3tvalue") {
		t.Fatalf("result leaked the token value: %+v", got)
	}
	if got := findResult(t, res, "discord token: security"); got.Status != Warn {
		t.Fatalf("security token unset should be Warn, got %v (%s)", got.Status, got.Detail)
	}
}

// containsSecret reports whether a result's rendered fields contain the value.
func containsSecret(r Result, secret string) bool {
	return strings.Contains(r.Detail, secret) || strings.Contains(r.Hint, secret)
}
