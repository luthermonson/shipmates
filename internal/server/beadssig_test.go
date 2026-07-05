package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBeadsStateSig(t *testing.T) {
	t.Chdir(t.TempDir())

	if sig := beadsStateSig(); sig != "" {
		t.Fatalf("no workspace should yield empty sig, got %q", sig)
	}

	manifest := filepath.Join(".beads", "embeddeddolt", "proj", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, []byte("root-hash-1"), 0o644); err != nil {
		t.Fatal(err)
	}
	sig1 := beadsStateSig()
	if sig1 == "" {
		t.Fatal("manifest present should yield a sig")
	}
	if sig2 := beadsStateSig(); sig2 != sig1 {
		t.Fatal("sig must be stable when content is unchanged")
	}

	if err := os.WriteFile(manifest, []byte("root-hash-2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if sig3 := beadsStateSig(); sig3 == sig1 {
		t.Fatal("content change must change the sig")
	}
}

func TestBeadsTriggerCoalesces(t *testing.T) {
	s := New()
	s.markBeadsDirty()
	s.markBeadsDirty() // second wake-up must not block
	select {
	case <-s.beadsTrigger:
	default:
		t.Fatal("trigger should hold a queued wake-up")
	}
	select {
	case <-s.beadsTrigger:
		t.Fatal("wake-ups must coalesce to one")
	default:
	}
	s.beadsMu.Lock()
	dirty := s.beadsDirty
	s.beadsMu.Unlock()
	if !dirty {
		t.Fatal("dirty flag should be set")
	}
}
