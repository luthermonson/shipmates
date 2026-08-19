package brig

import (
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/permissions"
)

// TestKernelArticle15HasWindowsParity is the Brig half of the #37 follow-up:
// Article 15 (stay-aboard) protected /etc, a Unix-only absolute path that
// matches nothing on Windows, leaving the "don't touch high-value OS locations
// outside the ship" intent Unix-only. The Windows-equivalent targets — the
// system tree C:\Windows and the per-user Startup autorun folder — must be
// refused too, while the existing Unix targets and ordinary project files are
// untouched. Forward slashes throughout so the fixture is honest on every OS:
// the matcher normalizes both spellings to the slash form, and a backslash on
// unix is an ordinary filename byte (the split TestKernelNormalizesWindowsSeparators
// pins), so a `C:\...` string there would be one odd basename, not a path.
func TestKernelArticle15HasWindowsParity(t *testing.T) {
	e := evalWithBrig(DefaultSettings())
	cases := []struct {
		name    string
		tool    string
		input   map[string]any
		want    permissions.Effect
		article string
	}{
		// Windows system tree — the C:\Windows analogue of /etc, including the
		// real hosts file that lives under it.
		{"C:/Windows write denied", "Write", fileInput("C:/Windows/System32/drivers/etc/hosts"), permissions.EffectDeny, "Article 15"},
		{"C:/Windows edit denied", "Edit", fileInput("C:/Windows/win.ini"), permissions.EffectDeny, "Article 15"},
		// Per-user Startup folder — an autorun-persistence foothold, matched at
		// any user-profile depth.
		{"startup folder write denied", "Write",
			fileInput("C:/Users/x/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/evil.lnk"),
			permissions.EffectDeny, "Article 15"},
		// The Unix side still bites — parity ADDS targets, it does not replace.
		{"/etc still denied", "Write", fileInput("/etc/hosts"), permissions.EffectDeny, "Article 15"},
		{".ssh still denied", "Write", fileInput("C:/Users/x/.ssh/authorized_keys"), permissions.EffectDeny, "Article 15"},

		// Negatives — the new targets must not become a blanket deny.
		{"project file under a Windows-named dir untouched", "Write", fileInput("src/windows/config.go"), permissions.EffectAllow, ""},
		{"project Startup-shaped path untouched", "Write", fileInput("docs/Startup/guide.md"), permissions.EffectAllow, ""},
		{"ordinary project file untouched", "Write", fileInput("main.go"), permissions.EffectAllow, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := e.EvaluateFor("backend", tc.tool, tc.input)
			if d.Effect != tc.want {
				t.Fatalf("%s %v => %s (%s), want %s", tc.tool, tc.input, d.Effect, d.Reason, tc.want)
			}
			if tc.article != "" && !strings.Contains(d.Reason, tc.article) {
				t.Errorf("reason %q does not name %s", d.Reason, tc.article)
			}
			if tc.article == "" && strings.Contains(d.Reason, "brig:") {
				t.Errorf("un-gated call attributed to the brig: %s", d.Reason)
			}
		})
	}
}

// TestKernelArticle15WindowsParityResolvesTraversal: a relative walk out of the
// ship that lands in C:\Windows names the same file the absolute target does,
// so MatchPathIn's root resolution must catch it too — the same guarantee the
// Unix /etc target already has.
func TestKernelArticle15WindowsParityResolvesTraversal(t *testing.T) {
	// The evaluator resolves relative paths against the project root; the brig
	// test seam does not set one, so assert the pattern directly through the
	// matcher with a Windows-shaped root, mirroring the permissions package's
	// TestMatchPathIn_ResolvesAgainstTheProjectRoot.
	const root = "C:/Users/x/proj"
	if !permissions.MatchPathIn("C:/Windows/**", "../../../Windows/System32/drivers/etc/hosts", root) {
		t.Error("a relative walk into C:/Windows was not resolved against the project root")
	}
	if permissions.MatchPathIn("C:/Windows/**", "src/main.go", root) {
		t.Error("an ordinary project file was matched by the C:/Windows target")
	}
}
