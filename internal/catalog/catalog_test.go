package catalog

import (
	"reflect"
	"testing"
	"testing/fstest"
)

// fakeCatalog builds an in-memory catalog FS with two personas. "bosun" ships an
// agent file plus two memory seeds; "navigator" ships only an agent file.
func fakeCatalog() fstest.MapFS {
	return fstest.MapFS{
		"catalog/bosun/.claude/agents/bosun.md": {
			Data: []byte("---\nname: bosun\n---\nbosun body\n"),
		},
		"catalog/bosun/memory-seeds/seed.md": {
			Data: []byte("seed one\n"),
		},
		"catalog/bosun/memory-seeds/log.md": {
			Data: []byte("seed two\n"),
		},
		"catalog/navigator/.claude/agents/navigator.md": {
			Data: []byte("---\nname: navigator\n---\nnav body\n"),
		},
	}
}

func TestPersonasSorted(t *testing.T) {
	c := New(fakeCatalog())
	got, err := c.Personas()
	if err != nil {
		t.Fatalf("Personas: %v", err)
	}
	want := []string{"bosun", "navigator"}
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
		{"bosun", true},
		{"navigator", true},
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
	got, err := c.AgentFile("bosun")
	if err != nil {
		t.Fatalf("AgentFile: %v", err)
	}
	want := "---\nname: bosun\n---\nbosun body\n"
	if string(got) != want {
		t.Fatalf("AgentFile(bosun) = %q, want %q", got, want)
	}

	if _, err := c.AgentFile("ghost"); err == nil {
		t.Fatal("AgentFile(ghost) error = nil, want not-exist error")
	}
}

func TestMemorySeeds(t *testing.T) {
	c := New(fakeCatalog())

	seeds, err := c.MemorySeeds("bosun")
	if err != nil {
		t.Fatalf("MemorySeeds: %v", err)
	}
	want := map[string][]byte{
		"seed.md": []byte("seed one\n"),
		"log.md":  []byte("seed two\n"),
	}
	if !reflect.DeepEqual(seeds, want) {
		t.Fatalf("MemorySeeds(bosun) = %v, want %v", seeds, want)
	}

	none, err := c.MemorySeeds("navigator")
	if err != nil {
		t.Fatalf("MemorySeeds(navigator): %v", err)
	}
	if none == nil {
		t.Fatal("MemorySeeds(navigator) = nil, want empty map")
	}
	if len(none) != 0 {
		t.Fatalf("MemorySeeds(navigator) len = %d, want 0", len(none))
	}
}
