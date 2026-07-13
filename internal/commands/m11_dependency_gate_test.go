package commands

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestM11ProductionDependencyAndReachabilityGate(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, dir := range []string{"internal/streamjson", "internal/permissions", "internal/fleet"} {
		if _, err := os.Stat(filepath.Join(root, dir)); !os.IsNotExist(err) {
			t.Fatalf("legacy production package remains: %s", dir)
		}
	}
	allowedLegacyRuntime := func(rel string) bool {
		return strings.HasPrefix(rel, "internal/legacyaudit/") || strings.HasPrefix(rel, "internal/project/migration") || rel == "internal/commands/migrate.go" || rel == "internal/commands/policy.go" || strings.HasPrefix(rel, "internal/catalog/")
	}
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(raw)
		for _, forbidden := range []string{`internal/streamjson"`, `internal/permissions"`, `internal/fleet"`, `exec.Command("legacyagent"`, `exec.CommandContext(ctx, "legacyagent"`, `LookPath("legacyagent"`} {
			if strings.Contains(text, forbidden) {
				t.Errorf("%s contains forbidden production reachability %q", rel, forbidden)
			}
		}
		if rel != "internal/project/migration.go" && strings.Contains(text, "LegacyLegacyRuntimeAgentPath(") {
			t.Errorf("%s calls migration-only legacy path helper", rel)
		}
		legacyCalls := []struct {
			name    string
			allowed map[string]bool
		}{
			{"BuildLegacyMigrationPlan", map[string]bool{"internal/project/migration.go": true, "internal/project/migration_apply.go": true, "internal/commands/migrate.go": true}},
			{"ApplyLegacyMigration", map[string]bool{"internal/project/migration_apply.go": true, "internal/commands/migrate.go": true}},
			{"legacyaudit.Build(", map[string]bool{"internal/commands/policy.go": true}},
		}
		for _, call := range legacyCalls {
			if strings.Contains(text, call.name) && !call.allowed[rel] {
				t.Errorf("%s calls migration-only entry point %s", rel, call.name)
			}
		}
		if !allowedLegacyRuntime(rel) && (strings.Contains(text, `".legacy-runtime`) || strings.Contains(strings.ToLower(text), "forbiddenprovider")) {
			t.Errorf("%s contains non-allowlisted legacy runtime reference", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
