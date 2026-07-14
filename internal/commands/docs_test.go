package commands

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPublicCommandsAreDocumented(t *testing.T) {
	root := documentationRoot(t)
	reference := readDocumentation(t, filepath.Join(root, "docs", "cli-reference.md"))

	commands := []string{
		"init", "policy", "add", "list", "remove", "update", "render",
		"routing", "open", "ask", "live", "tell", "feed", "interrupt",
		"fanout", "drain", "drain-many", "autonomous", "plan", "sail", "fleet", "server", "ship",
	}
	for _, command := range commands {
		if !strings.Contains(reference, "`shipmates "+command) {
			t.Errorf("public command %q is missing from docs/cli-reference.md", command)
		}
	}
}

func TestDocumentationSetAndCodexOnlyLanguage(t *testing.T) {
	root := documentationRoot(t)
	files := []string{
		"README.md",
		"docs/getting-started.md",
		"docs/sailing.md",
		"docs/cli-reference.md",
		"docs/configuration.md",
		"docs/operations.md",
		"docs/security.md",
		"docs/architecture.md",
		"docs/fleet-architecture.md",
		"docs/diagrams.md",
		"docs/PHILOSOPHY.md",
	}

	for _, name := range files {
		body := strings.ToLower(readDocumentation(t, filepath.Join(root, name)))
		for _, removed := range []string{"anthropic", "claude"} {
			if strings.Contains(body, removed) {
				t.Errorf("%s contains removed product language %q", name, removed)
			}
		}
	}
}

func documentationRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate documentation test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}

func readDocumentation(t *testing.T, name string) string {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(body)
}
