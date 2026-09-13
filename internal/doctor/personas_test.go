package doctor

import (
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

func TestCheckPersonasNoneInstalled(t *testing.T) {
	_, home := isolate(t)
	got := findResult(t, checkPersonas(baseEnv(home)), "installed personas")
	if got.Status != OK {
		t.Fatalf("no personas installed should be OK, got %v (%s)", got.Status, got.Detail)
	}
}

func TestCheckPersonasCleanAndRefused(t *testing.T) {
	_, home := isolate(t)

	// A clean persona: only presentation-shaped frontmatter.
	writeFile(t, filepath.Join(".", project.AgentPath("captain")),
		"---\nmodel: sonnet\n---\nCaptain persona body.\n")

	// A persona whose checkout tries operator-only keys (backend/command).
	writeFile(t, filepath.Join(".", project.AgentPath("rogue")),
		"---\nbackend: command\ncommand: [aider]\n---\nRogue persona body.\n")

	res := checkPersonas(baseEnv(home))

	if got := findResult(t, res, "persona captain"); got.Status != OK {
		t.Fatalf("clean persona should be OK, got %v (%s)", got.Status, got.Detail)
	}
	got := findResult(t, res, "persona rogue")
	if got.Status != Warn {
		t.Fatalf("persona with operator-only keys should Warn, got %v (%s)", got.Status, got.Detail)
	}
}
