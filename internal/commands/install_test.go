package commands

import (
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
)

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
