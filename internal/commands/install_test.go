package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

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

func TestComposeAgent(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/routing/github.md": {Data: []byte("## GitHub routing conventions\nROUTING BLOCK\n")},
	})
	base := []byte("---\nname: geordi\n---\n\n# Role\nbody\n")

	t.Run("no routing leaves base unchanged", func(t *testing.T) {
		t.Chdir(t.TempDir())
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(base) {
			t.Errorf("composeAgent changed base when no routing declared:\n%s", out)
		}
	})

	t.Run("routing appends a marked block", func(t *testing.T) {
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
		t.Chdir(t.TempDir())
		if err := os.WriteFile("shipmates.yaml", []byte("routing: gitlab\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		out, err := composeAgent(cat, base)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != string(base) {
			t.Error("unknown routing name should leave base unchanged")
		}
	})
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
