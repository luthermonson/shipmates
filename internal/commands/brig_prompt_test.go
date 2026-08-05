package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/catalog"
)

// disableBrig writes a user config turning the brig off, under a pinned home.
func disableBrig(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".shipmates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("brig:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestRunBrigReminder covers the SessionStart hook body: default posture
// emits the Articles context, an engaged freeze is announced loudly, and a
// disabled brig emits nothing at all.
func TestRunBrigReminder(t *testing.T) {
	pinHome(t)
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := runBrigReminder(&stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	s := stdout.String()
	if !strings.Contains(s, "SessionStart") || !strings.Contains(s, "Ship's Articles") {
		t.Errorf("reminder output = %q", s)
	}
	if strings.Contains(s, "FREEZE IS ENGAGED") {
		t.Error("announced a freeze that isn't engaged")
	}

	if err := brig.SetFreeze(".", "all stop", "luther"); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if err := runBrigReminder(&stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "FREEZE IS ENGAGED") || !strings.Contains(stdout.String(), "all stop") {
		t.Errorf("engaged freeze not announced: %q", stdout.String())
	}
}

func TestRunBrigReminderDisabledEmitsNothing(t *testing.T) {
	home := pinHome(t)
	disableBrig(t, home)
	t.Chdir(t.TempDir())
	var stdout, stderr bytes.Buffer
	if err := runBrigReminder(&stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Errorf("disabled brig still emitted context: %q", stdout.String())
	}
}

// brigTestCatalog carries just enough catalog for init to vendor the
// Articles document.
func brigTestCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/ARTICLES.md": {Data: []byte("# The Ship's Articles\n\nfifteen rules\n")},
	})
}

// TestInitInstallsArticlesDoc: `shipmates init` vendors the canonical
// Articles text so the persona reminder's pointer resolves in-project — and
// skips it when the operator has the brig off.
func TestInitInstallsArticlesDoc(t *testing.T) {
	pinHome(t)
	t.Chdir(t.TempDir())
	if err := Init(brigTestCatalog()).Run(context.Background(), []string{"init"}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(".shipmates/ARTICLES.md")
	if err != nil {
		t.Fatalf("init did not vendor ARTICLES.md: %v", err)
	}
	if !strings.Contains(string(body), "The Ship's Articles") {
		t.Errorf("vendored Articles doc looks wrong:\n%.200s", body)
	}
}

func TestInitSkipsArticlesDocWhenDisabled(t *testing.T) {
	home := pinHome(t)
	disableBrig(t, home)
	t.Chdir(t.TempDir())
	if err := Init(brigTestCatalog()).Run(context.Background(), []string{"init"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(".shipmates/ARTICLES.md"); !os.IsNotExist(err) {
		t.Errorf("disabled brig still vendored ARTICLES.md (err=%v)", err)
	}
}
