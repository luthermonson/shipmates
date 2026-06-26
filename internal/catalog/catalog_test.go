package catalog

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// fakeCatalog builds an in-memory catalog FS with two personas. "lead" ships an
// agent file plus two memory seeds; "tester" ships only an agent file.
func fakeCatalog() fstest.MapFS {
	return fstest.MapFS{
		"catalog/lead/.claude/agents/lead.md": {
			Data: []byte("---\nname: lead\n---\nlead body\n"),
		},
		"catalog/lead/memory-seeds/seed.md": {
			Data: []byte("seed one\n"),
		},
		"catalog/lead/memory-seeds/log.md": {
			Data: []byte("seed two\n"),
		},
		"catalog/tester/.claude/agents/tester.md": {
			Data: []byte("---\nname: tester\n---\ntester body\n"),
		},
	}
}

func TestPersonasSorted(t *testing.T) {
	c := New(fakeCatalog())
	got, err := c.Personas()
	if err != nil {
		t.Fatalf("Personas: %v", err)
	}
	want := []string{"lead", "tester"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Personas() = %v, want %v (sorted)", got, want)
	}
}

func TestHas(t *testing.T) {
	c := New(fakeCatalog())
	tests := []struct {
		name string
		want bool
	}{
		{"lead", true},
		{"tester", true},
		{"ghost", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := c.Has(tt.name); got != tt.want {
				t.Fatalf("Has(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestAgentFile(t *testing.T) {
	c := New(fakeCatalog())
	got, err := c.AgentFile("lead")
	if err != nil {
		t.Fatalf("AgentFile: %v", err)
	}
	want := "---\nname: lead\n---\nlead body\n"
	if string(got) != want {
		t.Fatalf("AgentFile(lead) = %q, want %q", got, want)
	}

	if _, err := c.AgentFile("ghost"); err == nil {
		t.Fatal("AgentFile(ghost) error = nil, want not-exist error")
	}
}

func TestMemorySeeds(t *testing.T) {
	c := New(fakeCatalog())

	seeds, err := c.MemorySeeds("lead")
	if err != nil {
		t.Fatalf("MemorySeeds: %v", err)
	}
	want := map[string][]byte{
		"seed.md": []byte("seed one\n"),
		"log.md":  []byte("seed two\n"),
	}
	if !reflect.DeepEqual(seeds, want) {
		t.Fatalf("MemorySeeds(lead) = %v, want %v", seeds, want)
	}

	none, err := c.MemorySeeds("tester")
	if err != nil {
		t.Fatalf("MemorySeeds(tester): %v", err)
	}
	if none == nil {
		t.Fatal("MemorySeeds(tester) = nil, want empty map")
	}
	if len(none) != 0 {
		t.Fatalf("MemorySeeds(tester) len = %d, want 0", len(none))
	}
}
