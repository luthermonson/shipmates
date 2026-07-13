//go:build !unix

package policy

func load(projectRoot, persona, projectRootID string) (*Snapshot, []Diagnostic) {
	return nil, loadDiagnostic("internal", SourceDescriptor{Layer: LayerProject, Path: ".shipmates/policy.yaml"}, "secure policy loading is unsupported on this platform")
}
