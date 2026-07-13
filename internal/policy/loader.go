package policy

// Load reads the complete project-local policy source set and assembles one
// semantic snapshot. Filesystem-specific implementations must open every path
// component without following symlinks and read each file from the descriptor
// that was validated as regular.
func Load(projectRoot, persona, projectRootID string) (*Snapshot, []Diagnostic) {
	return load(projectRoot, persona, projectRootID)
}

func loadDiagnostic(code string, d SourceDescriptor, message string) []Diagnostic {
	return []Diagnostic{{SchemaVersion: SchemaVersion, Code: code, Layer: d.Layer, Path: d.Path, Message: message}}
}
