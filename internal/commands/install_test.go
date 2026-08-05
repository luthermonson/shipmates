package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

// pinHome points os.UserHomeDir at a fresh temp dir so tests never read the
// developer's real ~/.shipmates/config.yaml — the brig posture under test is
// always the default (enabled) unless the test writes its own config.
func pinHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("USERPROFILE", home) // Windows
	t.Setenv("HOME", home)        // Unix
	return home
}

func TestEnsureAttachGitignore(t *testing.T) {
	t.Run("creates .gitignore when missing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(".gitignore")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), attachInboxIgnorePattern) {
			t.Errorf("pattern not written: %q", got)
		}
	})

	t.Run("appends to existing .gitignore without duplicating", func(t *testing.T) {
		t.Chdir(t.TempDir())
		orig := "node_modules/\n*.log\n"
		if err := os.WriteFile(".gitignore", []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		// Second call must not add a second entry.
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(".gitignore")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "node_modules/") || !strings.Contains(string(got), "*.log") {
			t.Errorf("existing entries dropped: %q", got)
		}
		if n := strings.Count(string(got), attachInboxIgnorePattern); n != 1 {
			t.Errorf("expected exactly one inbox pattern, got %d: %q", n, got)
		}
	})

	t.Run("no-op when pattern already present", func(t *testing.T) {
		t.Chdir(t.TempDir())
		orig := "# user comment\n.shipmates/inbox/\nother\n"
		if err := os.WriteFile(".gitignore", []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := ensureAttachGitignore(); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(".gitignore")
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != orig {
			t.Errorf("file changed when pattern already present:\nwant %q\ngot  %q", orig, got)
		}
	})
}

// stripArticles removes the brig's marker-delimited Articles reminder so
// routing-focused assertions can compare against the base file. The default
// operator posture splices the block into every composed persona; that
// composition has its own tests (TestComposeAgent_ArticlesBlock).
func stripArticles(b []byte) []byte {
	return []byte(brig.SplicePrompt(string(b), ""))
}

func TestComposeAgent(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/routing/github.md": {Data: []byte("## GitHub routing conventions\nROUTING BLOCK\n")},
	})
	base := []byte("---\nname: geordi\n---\n\n# Role\nbody\n")

	t.Run("no routing leaves base unchanged", func(t *testing.T) {
		pinHome(t)
		t.Chdir(t.TempDir())
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(stripArticles(out)) != string(base) {
			t.Errorf("composeAgent changed base when no routing declared:\n%s", out)
		}
	})

	t.Run("routing appends a marked block", func(t *testing.T) {
		pinHome(t)
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		s := string(out)
		if !strings.HasPrefix(s, "---\nname: geordi\n") {
			t.Error("base content not preserved at the top")
		}
		if !strings.Contains(s, "ROUTING BLOCK") {
			t.Error("routing block not appended")
		}
		if !strings.Contains(s, "<!-- shipmates:routing:github -->") || !strings.Contains(s, "<!-- /shipmates:routing:github -->") {
			t.Error("routing markers missing")
		}
	})

	t.Run("unknown routing name is a no-op", func(t *testing.T) {
		pinHome(t)
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: gitlab\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(stripArticles(out)) != string(base) {
			t.Error("unknown routing name should leave base unchanged")
		}
	})
}

// TestComposeAgent_ArticlesBlock pins the brig side of composition: the
// default posture appends the marker-delimited reminder after everything
// else, a disabled brig composes byte-identical to the base, and re-enabling
// plus recomposing brings the block back — the `shipmates update` re-enable
// path.
func TestComposeAgent_ArticlesBlock(t *testing.T) {
	cat := catalog.New(fstest.MapFS{})
	base := []byte("---\nname: geordi\n---\n\n# Role\nbody\n")

	home := pinHome(t)
	t.Chdir(t.TempDir())
	on, err := composeAgent(cat, base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(on), brig.PromptStartMarker) || !strings.Contains(string(on), "Ship's Articles") {
		t.Fatalf("default posture: composed persona lacks the Articles reminder:\n%s", on)
	}
	// Composing the already-composed output is stable (update recomposition).
	again, err := composeAgent(cat, on)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(on) {
		t.Errorf("recomposition not idempotent:\n--- first\n%s\n--- second\n%s", on, again)
	}

	// Disable the brig: composition returns to the bare base, block removed.
	confDir := filepath.Join(home, ".shipmates")
	if err := os.MkdirAll(confDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(confDir, "config.yaml"), []byte("brig:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	off, err := composeAgent(cat, on)
	if err != nil {
		t.Fatal(err)
	}
	if string(off) != string(base) {
		t.Errorf("disabled brig should compose back to the base:\n--- got\n%s\n--- want\n%s", off, base)
	}

	// Re-enable: the block comes back.
	if err := os.WriteFile(filepath.Join(confDir, "config.yaml"), []byte("brig:\n  enabled: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	back, err := composeAgent(cat, off)
	if err != nil {
		t.Fatal(err)
	}
	if string(back) != string(on) {
		t.Errorf("re-enable did not restore the composed form:\n--- got\n%s\n--- want\n%s", back, on)
	}
}

func TestAddPersona_VendorsPolicyYAML(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
		"catalog/geordi/policy.yaml": {Data: []byte("allow:\n  - Bash(git status)\ndeny:\n  - Bash(rm -rf /)\n")},
	})
	t.Chdir(t.TempDir())
	// Prep the layout addPersona expects.
	if err := os.MkdirAll(project.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	// Policy should have landed under .shipmates/policies/.
	polPath := filepath.Join(".shipmates", "policies", "geordi.yaml")
	b, err := os.ReadFile(polPath)
	if err != nil {
		t.Fatalf("expected vendored policy at %s: %v", polPath, err)
	}
	if !strings.Contains(string(b), "Bash(rm -rf /)") {
		t.Errorf("vendored policy missing content: %s", string(b))
	}
}

func TestAddPersona_NoPolicyYAMLIsFine(t *testing.T) {
	// Personas without a policy.yaml must install cleanly and NOT create an
	// empty policy file — that would be a footgun for operators wondering
	// what's in the empty file.
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
	})
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(project.AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := addPersona(cat, "geordi"); err != nil {
		t.Fatalf("addPersona: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".shipmates", "policies", "geordi.yaml")); !os.IsNotExist(err) {
		t.Errorf("no-policy persona should not create a policy file (err=%v)", err)
	}
}
