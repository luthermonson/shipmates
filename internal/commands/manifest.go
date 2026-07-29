package commands

import (
	"fmt"
	"log/slog"

	"github.com/luthermonson/shipmates/internal/project"
)

// requireManifestV2 makes the install manifest usable for a lifecycle
// operation, migrating it first when it predates manifest v2.
//
// Releases before the versioned manifest wrote `"version": ""`, so gating
// init/add/update/remove on version == "2" with no migration made every
// project created before then permanently unusable: all four operations failed
// and the only recovery was hand-editing .shipmates/manifest.json. The recorded
// file hashes are still valid across that boundary — the legacy `.claude`
// artifact paths they name are a live runtime layout — so the upgrade is a
// version stamp plus an atomic republish, never a reset.
func requireManifestV2(manifest *project.Manifest, operation string) error {
	migrated, err := project.MigrateManifest(".", manifest)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if migrated {
		slog.Info("upgraded legacy install manifest to v2", "path", project.ManifestPath(), "operation", operation)
	}
	if manifest.Version != project.ManifestVersion || manifest.Files == nil {
		return fmt.Errorf("%s requires a valid manifest v2", operation)
	}
	return nil
}
