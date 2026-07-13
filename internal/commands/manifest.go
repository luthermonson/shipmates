package commands

import (
	"fmt"

	"github.com/luthermonson/shipmates/internal/project"
)

func requireManifestV2(manifest *project.Manifest, operation string) error {
	if manifest == nil || manifest.Version != project.ManifestVersion || manifest.Files == nil {
		return fmt.Errorf("%s requires a valid manifest v2", operation)
	}
	return nil
}
