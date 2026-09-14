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

func TestCheckProjectConfigAbsentPresentMalformed(t *testing.T) {
	proj, home := isolate(t)
	e := baseEnv(home)

	// Absent -> informational OK.
	if got := findResult(t, checkProjectConfig(e), "project config (shipmates.yaml)"); got.Status != OK {
		t.Fatalf("absent shipmates.yaml should be OK, got %v (%s)", got.Status, got.Detail)
	}

	// Present + clean -> OK.
	writeFile(t, filepath.Join(proj, "shipmates.yaml"), "captainPersona: skipper\n")
	if got := findResult(t, checkProjectConfig(e), "project config (shipmates.yaml)"); got.Status != OK {
		t.Fatalf("clean shipmates.yaml should be OK, got %v (%s)", got.Status, got.Detail)
	}

	// Present + malformed -> FAIL.
	writeFile(t, filepath.Join(proj, "shipmates.yaml"), "crew: [unclosed\n")
	if got := findResult(t, checkProjectConfig(e), "project config (shipmates.yaml)"); got.Status != Fail {
		t.Fatalf("malformed shipmates.yaml should be Fail, got %v (%s)", got.Status, got.Detail)
	}
}

// TestMalformedProjectConfigYieldsExactlyOneFail is the N1 invariant: a broken
// shipmates.yaml produces exactly one FAIL, correctly labeled under Config — no
// misleading "fleet config" FAIL under Fleet, no double-FAIL on runtime
// selection, and no bogus Beads verdict derived from a file that won't parse.
func TestMalformedProjectConfigYieldsExactlyOneFail(t *testing.T) {
	proj, home := isolate(t)
	writeFile(t, filepath.Join(proj, "shipmates.yaml"), "crew: [unclosed\n")

	e := baseEnv(home) // claude + git present, so Toolchain does not FAIL

	results := Run(e)

	var fails []Result
	for _, r := range results {
		if r.Status == Fail {
			fails = append(fails, r)
		}
		if r.Group == "Fleet" {
			t.Fatalf("a malformed shipmates.yaml must not emit any Fleet-group result, got %+v", r)
		}
	}
	if len(fails) != 1 {
		t.Fatalf("want exactly one FAIL, got %d: %+v", len(fails), fails)
	}
	only := fails[0]
	if only.Name != "project config (shipmates.yaml)" || only.Group != "Config" {
		t.Fatalf("the single FAIL should be the Config-group project-config check, got %+v", only)
	}
	// Runtime selection reads .shipmates/config.yaml (a different file) and must
	// still be OK here — it must not re-FAIL on the shipmates.yaml parse error.
	if got := findResult(t, results, "runtime selection"); got.Status == Fail {
		t.Fatalf("runtime selection must not FAIL on a shipmates.yaml parse error, got %+v", got)
	}
	// Beads must not turn the parse fault into a FAIL or a bogus verdict.
	if got := findResult(t, results, "bd (Beads)"); got.Status == Fail {
		t.Fatalf("bd (Beads) must not FAIL on a shipmates.yaml parse error, got %+v", got)
	}
}

// TestCheckConfigFilesUnresolvableHomeSkipsShipYaml is the N5 invariant: when
// the home directory cannot be resolved, ship.yaml follows the same ok-guarded
// pattern as the other operator files and is omitted, instead of silently
// stat-ing a cwd-relative .shipmates/ship.yaml (a different project's file).
func TestCheckConfigFilesUnresolvableHomeSkipsShipYaml(t *testing.T) {
	proj, _ := isolate(t)
	// Home unresolvable: no Env.Home override AND no HOME/USERPROFILE.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	// A cwd-relative .shipmates/ship.yaml the old unguarded path would have
	// stat-ed and reported as though it were the operator's ship.yaml.
	writeFile(t, filepath.Join(proj, ".shipmates", "ship.yaml"), "projects:\n  - /somewhere\n")

	e := baseEnv("") // Home == "" -> homeDir() falls back to os.UserHomeDir, which now fails
	for _, r := range checkConfigFiles(e) {
		if r.Name == "~/.shipmates/ship.yaml" {
			t.Fatalf("ship.yaml check must be omitted when home is unresolvable, got %+v", r)
		}
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

// containsSecretIn reports whether ANY result's fields contain the value. The
// per-result containsSecret only inspects one finding; a full no-leak assertion
// must scan every result the run produced.
func containsSecretIn(results []Result, secret string) bool {
	for _, r := range results {
		if containsSecret(r, secret) {
			return true
		}
	}
	return false
}
