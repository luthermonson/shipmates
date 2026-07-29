package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRawManifest drops literal manifest bytes into a project root, the way a
// release that predates manifest v2 left them behind.
func writeRawManifest(t *testing.T, root, raw string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, Dir), 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, ManifestPath())
	if err := os.WriteFile(p, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func loadManifestAt(t *testing.T, root string) *Manifest {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, ManifestPath()))
	if err != nil {
		t.Fatal(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	return &m
}

const legacyManifestSHA = "1111111111111111111111111111111111111111111111111111111111111111"

// Every project created before the manifest gained a version carries
// `"version": ""`, and the v2 gate rejected all of them with no migration and no
// recovery path. Such a manifest must be upgraded in place, and the recorded
// hashes — the only record of which artifacts shipmates owns — must survive
// verbatim, including legacy `.claude` artifact paths.
func TestMigrateManifestUpgradesLegacyManifestInPlace(t *testing.T) {
	root := t.TempDir()
	path := writeRawManifest(t, root, `{
  "version": "",
  "files": {
    ".claude/agents/geordi.md": "`+legacyManifestSHA+`",
    ".shipmates/policies/geordi.yaml": "`+legacyManifestSHA+`"
  }
}`)
	m := loadManifestAt(t, root)
	if m.Version != "" {
		t.Fatalf("fixture is not a legacy manifest: version = %q", m.Version)
	}

	migrated, err := MigrateManifest(root, m)
	if err != nil {
		t.Fatalf("migrate legacy manifest: %v", err)
	}
	if !migrated {
		t.Fatal("legacy manifest reported as needing no migration")
	}
	if m.Version != ManifestVersion {
		t.Fatalf("in-memory version = %q, want %q", m.Version, ManifestVersion)
	}
	if len(m.Files) != 2 || m.Files[".claude/agents/geordi.md"] != legacyManifestSHA {
		t.Fatalf("files were not carried across: %#v", m.Files)
	}

	// The upgrade must be durable and re-loadable, not just in memory.
	onDisk := loadManifestAt(t, root)
	if onDisk.Version != ManifestVersion {
		t.Fatalf("%s still reports version %q", path, onDisk.Version)
	}
	if len(onDisk.Files) != 2 || onDisk.Files[".shipmates/policies/geordi.yaml"] != legacyManifestSHA {
		t.Fatalf("%s lost tracked files: %#v", path, onDisk.Files)
	}
	// And it must satisfy the gate the lifecycle commands apply.
	if _, err := SerializeManifestV2(onDisk); err != nil {
		t.Fatalf("migrated manifest is not valid v2: %v", err)
	}

	// Idempotent: a second pass is a no-op and does not rewrite the file.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	migrated, err = MigrateManifest(root, m)
	if err != nil || migrated {
		t.Fatalf("second migration = %v, %v; want false, nil", migrated, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a no-op migration rewrote the manifest")
	}
}

// Manifests written on Windows contain backslash separators because their keys
// came from filepath.Join. Validation must not reject them.
func TestMigrateManifestAcceptsWindowsSeparatedKeys(t *testing.T) {
	root := t.TempDir()
	writeRawManifest(t, root, `{"version":"","files":{".claude\\agents\\geordi.md":"`+legacyManifestSHA+`"}}`)
	m := loadManifestAt(t, root)
	migrated, err := MigrateManifest(root, m)
	if err != nil || !migrated {
		t.Fatalf("migrate = %v, %v; want true, nil", migrated, err)
	}
	if m.Files[`.claude\agents\geordi.md`] != legacyManifestSHA {
		t.Fatalf("windows key not preserved: %#v", m.Files)
	}
}

func TestMigrateManifestLeavesCurrentManifestAlone(t *testing.T) {
	root := t.TempDir()
	path := writeRawManifest(t, root, `{"version":"2","files":{"a.toml":"`+legacyManifestSHA+`"}}`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	m := loadManifestAt(t, root)
	migrated, err := MigrateManifest(root, m)
	if err != nil || migrated {
		t.Fatalf("migrate = %v, %v; want false, nil", migrated, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a current manifest was rewritten")
	}
}

// A fresh project has no manifest yet; there is nothing to republish and the
// migration must not conjure one.
func TestMigrateManifestDoesNotCreateAManifestForAFreshProject(t *testing.T) {
	root := t.TempDir()
	m := &Manifest{Files: map[string]string{}}
	migrated, err := MigrateManifest(root, m)
	if err != nil || !migrated {
		t.Fatalf("migrate = %v, %v; want true, nil", migrated, err)
	}
	if m.Version != ManifestVersion {
		t.Fatalf("version = %q", m.Version)
	}
	if _, err := os.Stat(filepath.Join(root, ManifestPath())); !os.IsNotExist(err) {
		t.Fatalf("migration created a manifest for a project without one: %v", err)
	}
}

// Anything the migration cannot vouch for must produce an actionable error and
// must leave the file on disk untouched, never a rewritten authoritative v2.
func TestMigrateManifestRefusesWhatItCannotSafelyUpgrade(t *testing.T) {
	cases := []struct {
		name, raw, want string
	}{
		{"future version", `{"version":"3","files":{}}`, "does not understand"},
		{"malformed hash", `{"version":"","files":{"a.toml":"nope"}}`, "malformed hash"},
		{"absolute path", `{"version":"","files":{"/etc/passwd":"` + legacyManifestSHA + `"}}`, "unusable path"},
		{"traversal path", `{"version":"","files":{"../../x.toml":"` + legacyManifestSHA + `"}}`, "unusable path"},
		{"empty path", `{"version":"","files":{"":"` + legacyManifestSHA + `"}}`, "unusable path"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			path := writeRawManifest(t, root, tt.raw)
			m := loadManifestAt(t, root)
			migrated, err := MigrateManifest(root, m)
			if err == nil {
				t.Fatalf("migrate succeeded (migrated = %v)", migrated)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), ManifestName) {
				t.Fatalf("error = %v, want it to name the manifest file", err)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if string(after) != tt.raw {
				t.Fatalf("a refused migration rewrote the manifest: %s", after)
			}
		})
	}

	if _, err := MigrateManifest(t.TempDir(), nil); err == nil {
		t.Fatal("nil manifest accepted")
	}
	root := t.TempDir()
	writeRawManifest(t, root, `{"version":"2"}`)
	if _, err := MigrateManifest(root, loadManifestAt(t, root)); err == nil || !strings.Contains(err.Error(), "files map") {
		t.Fatalf("v2 manifest with no files map: %v", err)
	}
}
