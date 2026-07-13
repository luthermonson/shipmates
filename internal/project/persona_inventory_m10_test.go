//go:build unix

package project

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeM10Agent(t *testing.T, root, name string, raw []byte) string {
	t.Helper()
	p := filepath.Join(root, CodexAgentPath(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if raw == nil {
		raw = []byte("name = " + strconv.Quote(name) + "\ndeveloper_instructions = \"test\"\n")
	}
	if err := os.WriteFile(p, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestM10CanonicalInventoryIsSortedBoundedAndIgnoresLegacyRuntime(t *testing.T) {
	root := t.TempDir()
	writeM10Agent(t, root, "zeta", nil)
	writeM10Agent(t, root, "alpha", nil)
	if err := os.MkdirAll(filepath.Join(root, ".legacy-runtime", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".legacy-runtime", "agents", "legacy.md"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := InstalledPersonasAt(root)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "alpha,zeta" {
		t.Fatalf("inventory = %v", got)
	}

	legacyOnly := t.TempDir()
	if err := os.MkdirAll(filepath.Join(legacyOnly, ".legacy-runtime", "agents"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyOnly, ".legacy-runtime", "agents", "legacy.md"), []byte("legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := InstalledPersonasAt(legacyOnly); err != nil || len(got) != 0 {
		t.Fatalf("legacy-only inventory = %v, %v", got, err)
	}

	over := t.TempDir()
	for i := 0; i <= MaxInstalledPersonas; i++ {
		writeM10Agent(t, over, "p"+strconv.Itoa(i), nil)
	}
	if _, err := InstalledPersonasAt(over); err == nil || !strings.Contains(err.Error(), "exceeds limit") {
		t.Fatalf("over-limit error = %v", err)
	}
}

func TestM10CanonicalInventoryRefusesUnsafeNodes(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, string)
	}{
		{"agents symlink", func(t *testing.T, root string) {
			target := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, ".codex"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, ".codex", "agents")); err != nil {
				t.Fatal(err)
			}
		}},
		{"leaf symlink", func(t *testing.T, root string) {
			p := writeM10Agent(t, root, "real", nil)
			if err := os.Symlink(p, filepath.Join(root, ".codex", "agents", "alias.toml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"special leaf", func(t *testing.T, root string) {
			p := writeM10Agent(t, root, "bad", nil)
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"hardlink leaf", func(t *testing.T, root string) {
			p := writeM10Agent(t, root, "one", nil)
			if err := os.Link(p, filepath.Join(root, ".codex", "agents", "two.toml")); err != nil {
				t.Fatal(err)
			}
		}},
		{"group-writable leaf", func(t *testing.T, root string) {
			p := writeM10Agent(t, root, "open", nil)
			if err := os.Chmod(p, 0o660); err != nil {
				t.Fatal(err)
			}
		}},
		{"owner-executable leaf", func(t *testing.T, root string) {
			p := writeM10Agent(t, root, "executable", nil)
			if err := os.Chmod(p, 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{"malformed leaf", func(t *testing.T, root string) { writeM10Agent(t, root, "bad", []byte("developer_instructions = [")) }},
		{"unsafe name", func(t *testing.T, root string) { writeM10Agent(t, root, "UPPER", nil) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			tt.mutate(t, root)
			if _, err := InstalledPersonasAt(root); err == nil {
				t.Fatal("unsafe inventory accepted")
			}
		})
	}
}
