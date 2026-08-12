package beads

import (
	"strings"
	"testing"
)

func lookupEnv(env []string, name string) (string, bool) {
	for _, item := range env {
		if key, value, ok := strings.Cut(item, "="); ok && strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

// TestBoundedEnvironmentMatchesNamesCaseInsensitively pins a bug that only
// appeared in native Windows shells. The allowlist deliberately includes
// SystemRoot and windir, but cmd and PowerShell export them in mixed case
// while the match was exact-uppercase, so both were dropped from every child
// bd process. The parent environment is passed explicitly here rather than
// read from the process: the first version of this test called t.Setenv and
// passed against the broken code, because Go's Setenv is case-insensitive on
// Windows and Git Bash had already exported the names in upper case.
func TestBoundedEnvironmentMatchesNamesCaseInsensitively(t *testing.T) {
	parent := []string{
		`SystemRoot=C:\WINDOWS`,
		`windir=C:\WINDOWS`,
		`Path=C:\Program Files\Git\cmd`,
		`UserProfile=C:\Users\probe`,
		`Temp=C:\Temp`,
	}
	got := boundedEnvironmentFrom(parent)
	for _, want := range []struct{ name, value string }{
		{"SystemRoot", `C:\WINDOWS`},
		{"windir", `C:\WINDOWS`},
		{"Path", `C:\Program Files\Git\cmd`},
		{"UserProfile", `C:\Users\probe`},
		{"Temp", `C:\Temp`},
	} {
		value, ok := lookupEnv(got, want.name)
		if !ok {
			t.Errorf("%s was dropped from the bd environment; the allowlist must match names case-insensitively because native Windows shells do not spell them uppercase", want.name)
			continue
		}
		if value != want.value {
			t.Errorf("%s = %q, want %q", want.name, value, want.value)
		}
	}
}

// USERPROFILE is Windows' HOME. Without it bd cannot resolve a home directory,
// so it reads neither its own config nor the user's global git identity.
func TestBoundedEnvironmentCarriesWindowsHome(t *testing.T) {
	got := boundedEnvironmentFrom([]string{`USERPROFILE=C:\Users\probe`})
	if value, ok := lookupEnv(got, "USERPROFILE"); !ok || value != `C:\Users\probe` {
		t.Fatalf("USERPROFILE = %q present=%v, want it passed through to bd", value, ok)
	}
}

// The allowlist exists to withhold credentials from an external binary, so the
// Windows credential-store directories stay out even though a normal bd
// invocation would have them, and so does anything unrecognized.
func TestBoundedEnvironmentWithholdsCredentialDirectories(t *testing.T) {
	got := boundedEnvironmentFrom([]string{
		"APPDATA=must-not-reach-bd",
		"LOCALAPPDATA=must-not-reach-bd",
		"AppData=must-not-reach-bd",
		"GITHUB_TOKEN=must-not-reach-bd",
		"AWS_SECRET_ACCESS_KEY=must-not-reach-bd",
		"SHIPMATES_FLEET_TOKEN=must-not-reach-bd",
	})
	for _, item := range got {
		if strings.Contains(item, "must-not-reach-bd") {
			t.Fatalf("credential environment forwarded to bd: %s", item)
		}
	}
	if _, ok := lookupEnv(got, "BD_NON_INTERACTIVE"); !ok {
		t.Fatal("BD_NON_INTERACTIVE must always be set")
	}
}
