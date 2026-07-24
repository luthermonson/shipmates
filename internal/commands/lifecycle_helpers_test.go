package commands

import (
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
)

// lifecycleCatalog and lifecyclePolicy are cross-platform test fixtures used
// by lifecycle_m2_test.go (unix-only), policy_lock_unix_test.go (unix-only),
// and m11_release_gate_test.go (cross-platform). They live in an untagged
// _test.go file so the m11 release-gate suite still compiles on Windows.
func lifecycleCatalog(role, policy string) *catalog.Catalog {
	fsys := fstest.MapFS{
		"catalog/security/agent.md":              {Data: []byte("---\nname: security\ndescription: Security.\n---\n\n" + role + "\n")},
		"catalog/security/memory-seeds/notes.md": {Data: []byte("seed\n")},
	}
	if policy != "" {
		fsys["catalog/security/policy.yaml"] = &fstest.MapFile{Data: []byte(policy)}
	}
	return catalog.New(fsys)
}

func lifecyclePolicy(command string) string {
	return "version: 1\nallow:\n  - id: test.allow\n    kind: process.exec\n    match:\n      command_exact: \"" + command + "\"\n    reason: test policy\nask: []\ndeny: []\n"
}
