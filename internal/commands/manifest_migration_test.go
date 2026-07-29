package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

const legacyGateSHA = "2222222222222222222222222222222222222222222222222222222222222222"

// writeLegacyGateManifest installs a manifest with the given literal bytes in
// the current working directory's project dir.
func writeLegacyGateManifest(t *testing.T, raw string) string {
	t.Helper()
	if err := os.MkdirAll(project.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.ManifestPath(), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return project.ManifestPath()
}

// Every project created before the manifest gained a version has
// `"version": ""` on disk. The v2 gate rejected all of them, so init, add,
// update and remove all failed with no recovery path other than hand-editing
// .shipmates/manifest.json. The gate must migrate instead of refusing.
func TestRequireManifestV2MigratesLegacyProjectsForEveryLifecycleOperation(t *testing.T) {
	for _, operation := range []string{"init", "add", "update", "remove"} {
		t.Run(operation, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := writeLegacyGateManifest(t, `{"version":"","files":{".claude/agents/geordi.md":"`+legacyGateSHA+`"}}`)

			m, err := project.LoadManifest()
			if err != nil {
				t.Fatal(err)
			}
			if m.Version != "" {
				t.Fatalf("fixture is not legacy: version = %q", m.Version)
			}
			if err := requireManifestV2(m, operation); err != nil {
				t.Fatalf("%s refused a legacy project: %v", operation, err)
			}
			if m.Version != project.ManifestVersion {
				t.Fatalf("version = %q, want %q", m.Version, project.ManifestVersion)
			}
			if m.Files[".claude/agents/geordi.md"] != legacyGateSHA {
				t.Fatalf("the tracked artifact was dropped: %#v", m.Files)
			}

			// The upgrade is durable, so the next command sees a v2 manifest.
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var onDisk project.Manifest
			if err := json.Unmarshal(raw, &onDisk); err != nil {
				t.Fatal(err)
			}
			if onDisk.Version != project.ManifestVersion || onDisk.Files[".claude/agents/geordi.md"] != legacyGateSHA {
				t.Fatalf("%s was not upgraded on disk: %s", path, raw)
			}
			reloaded, err := project.LoadManifest()
			if err != nil {
				t.Fatal(err)
			}
			if err := requireManifestV2(reloaded, operation); err != nil {
				t.Fatalf("%s refused the migrated manifest: %v", operation, err)
			}
		})
	}
}

// A manifest the gate cannot vouch for must say what to do about it, and must
// not be rewritten into an authoritative v2 record.
func TestRequireManifestV2GivesAnActionableErrorItCannotMigrate(t *testing.T) {
	t.Chdir(t.TempDir())
	raw := `{"version":"9","files":{}}`
	path := writeLegacyGateManifest(t, raw)
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	err = requireManifestV2(m, "update")
	if err == nil {
		t.Fatal("an unrecognised manifest version was accepted")
	}
	for _, want := range []string{"update", project.ManifestName, "upgrade shipmates"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want it to mention %q", err, want)
		}
	}
	after, readErr := os.ReadFile(filepath.Clean(path))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != raw {
		t.Fatalf("a refused manifest was rewritten: %s", after)
	}
}
