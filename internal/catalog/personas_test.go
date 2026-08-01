package catalog

import (
	"testing"
	"testing/fstest"
)

// personaFS builds a catalog with two real personas and the shared resource
// directories the real one carries, so the distinction under test is the same
// one that exists on disk.
func personaFS() fstest.MapFS {
	return fstest.MapFS{
		"catalog/backend/.claude/agents/backend.md": {Data: []byte("# backend")},
		"catalog/captain/.claude/agents/captain.md": {Data: []byte("# captain")},
		"catalog/backend/policy.yaml":               {Data: []byte("{}")},
		"catalog/charters/autonomous.md":            {Data: []byte("# charter")},
		"catalog/charters/drain.md":                 {Data: []byte("# charter")},
		"catalog/commands/standup.md":               {Data: []byte("# command")},
		"catalog/routing/github.md":                 {Data: []byte("# routing")},
		"catalog/skills/captain/SKILL.md":           {Data: []byte("# skill")},
		"catalog/README.md":                         {Data: []byte("not a dir")},
	}
}

// TestPersonasExcludesResourceDirectories pins a real user-facing bug: every
// subdirectory of catalog/ was reported as a persona, so `shipmates list`
// offered charters, commands, routing and skills — none of which can be added.
func TestPersonasExcludesResourceDirectories(t *testing.T) {
	got, err := New(personaFS()).Personas()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"backend", "captain"}
	if len(got) != len(want) {
		t.Fatalf("Personas() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Personas() = %v, want %v", got, want)
		}
	}
}

// Has gates `add`. It previously accepted any directory, so `add charters` got
// past validation and died on a raw file-not-found from deeper in the command.
func TestHasRejectsResourceDirectories(t *testing.T) {
	c := New(personaFS())
	for _, name := range []string{"backend", "captain"} {
		if !c.Has(name) {
			t.Errorf("Has(%q) = false, want true — it is a real persona", name)
		}
	}
	for _, name := range []string{"charters", "commands", "routing", "skills"} {
		if c.Has(name) {
			t.Errorf("Has(%q) = true, want false — it is a shared resource directory, "+
				"and accepting it lets `add %s` through to a file-not-found", name, name)
		}
	}
	for _, name := range []string{"nonexistent", "README.md"} {
		if c.Has(name) {
			t.Errorf("Has(%q) = true, want false", name)
		}
	}
}

// A directory whose agent file is named after a different persona is not that
// persona. Guards against matching on "has any .claude/agents/*.md".
func TestIsPersonaRequiresMatchingAgentFile(t *testing.T) {
	c := New(fstest.MapFS{
		"catalog/skills/.claude/agents/captain.md": {Data: []byte("# borrowed")},
	})
	if c.Has("skills") {
		t.Error("Has(skills) = true, but skills only carries another persona's agent file")
	}
}
