package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

func TestCodexArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(project.AgentPath("security")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.AgentPath("security"), []byte("---\nname: security\n---\n\n# Role\n\nReview carefully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh, err := codexArgs("security", "check this", true, "", project.PersonaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fresh[:4], " "); got != "exec --json --sandbox workspace-write" {
		t.Fatalf("fresh args = %q", got)
	}
	if !strings.Contains(fresh[len(fresh)-1], ".shipmates/memory/security/") {
		t.Fatalf("fresh prompt does not preserve memory: %q", fresh[len(fresh)-1])
	}

	resume, err := codexArgs("security", "continue", false, "thread-123", project.PersonaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resume, " "); got != "exec resume thread-123 --json continue" {
		t.Fatalf("resume args = %q", got)
	}
}

func TestParseCodexEvent(t *testing.T) {
	id, text, err := parseCodexEvent([]byte(`{"type":"thread.started","thread_id":"thread-123"}`))
	if id != "thread-123" || text != "" || err != "" {
		t.Fatalf("thread event = %q, %q, %q", id, text, err)
	}
	_, text, err = parseCodexEvent([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`))
	if text != "done" || err != "" {
		t.Fatalf("item event = %q, %q", text, err)
	}
}
