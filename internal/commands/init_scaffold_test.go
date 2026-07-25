package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

func initTestCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/security/agent.md":    {Data: []byte("---\nname: security\ndescription: Security review.\n---\n\nReview carefully.\n")},
		"catalog/security/policy.yaml": {Data: []byte(emptyStrictPolicy)},
	})
}

// TestInitLeavesNothingBehindWhenItFails is the regression guard for the
// partial-artifact leak: an init that could not take the policy write lock
// used to return an error but leave a populated .shipmates/ and .codex/
// behind, so the operator was left with a project that looked initialized and
// was not.
func TestInitLeavesNothingBehindWhenItFails(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	original := acquirePolicyWriteLock
	acquirePolicyWriteLock = func(string) (*project.PolicyWriteLock, error) {
		return nil, errors.New("policy directory cannot be locked safely for mutation")
	}
	t.Cleanup(func() { acquirePolicyWriteLock = original })

	err := Init(initTestCatalog()).Run(context.Background(), []string{"shipmates"})
	if err == nil {
		t.Fatal("init succeeded with an unusable policy lock")
	}
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, e := range entries {
		t.Errorf("failed init left %q behind", e.Name())
	}
}

// TestInitRollbackKeepsPreexistingArtifacts proves the rollback only removes
// what init created. A project that already had .shipmates/ with operator
// content must come out of a failed init exactly as it went in.
func TestInitRollbackKeepsPreexistingArtifacts(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.MkdirAll(filepath.Join(project.Dir, project.MemoryDirName), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(project.Dir, project.MemoryDirName, "notes.md")
	if err := os.WriteFile(keep, []byte("operator notes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	original := acquirePolicyWriteLock
	acquirePolicyWriteLock = func(string) (*project.PolicyWriteLock, error) {
		return nil, errors.New("policy directory cannot be locked safely for mutation")
	}
	t.Cleanup(func() { acquirePolicyWriteLock = original })

	if err := Init(initTestCatalog()).Run(context.Background(), []string{"shipmates"}); err == nil {
		t.Fatal("init succeeded with an unusable policy lock")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("rollback destroyed pre-existing content: %v", err)
	}
	// .shipmates predates init, so it survives; the subdirectories init added
	// under it, and .codex, do not.
	for _, gone := range []string{project.PoliciesDir(), filepath.Join(project.Dir, project.SessionsDirName), ".codex"} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Errorf("failed init left %q behind: %v", gone, err)
		}
	}
}

// TestInitSucceedsAndRollsBackNothing keeps the happy path honest: a
// successful init must leave every artifact in place.
func TestInitSucceedsAndRollsBackNothing(t *testing.T) {
	skipIfNoPolicyLock(t)
	root := t.TempDir()
	t.Chdir(root)
	if err := Init(initTestCatalog()).Run(context.Background(), []string{"shipmates", "--crew", "security"}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		project.Dir,
		filepath.Join(project.Dir, "policy.yaml"),
		project.PoliciesDir(),
		filepath.Join(project.Dir, project.MemoryDirName),
		filepath.Join(project.Dir, project.SessionsDirName),
		project.CodexAgentsDir,
		project.ConfigName,
		project.CodexAgentPath("security"),
		project.PolicyPath("security"),
	} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("successful init is missing %q: %v", want, err)
		}
	}
}

// TestInitRollsBackAFailedCrewInstall covers the later failure point: the
// crew loop runs after the project has been scaffolded and the manifest
// saved, and an unknown persona there must still leave nothing behind.
func TestInitRollsBackAFailedCrewInstall(t *testing.T) {
	skipIfNoPolicyLock(t)
	root := t.TempDir()
	t.Chdir(root)
	if err := Init(initTestCatalog()).Run(context.Background(), []string{"shipmates", "--crew", "nosuchpersona"}); err == nil {
		t.Fatal("init accepted an unknown persona")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		t.Errorf("failed init left %q behind", e.Name())
	}
}
