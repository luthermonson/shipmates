//go:build unix

// Every test in this file exercises Init/Add/Update/Remove, each of which
// calls withPolicyWriteLock backed by unix flock (see
// internal/project/policylock_unix.go). Until the Windows LockFileEx port
// lands, these lifecycle scenarios cannot execute cross-platform.

package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

const legacyCaptainPolicy = "allow:\n  - Bash(git status)\nask: []\ndeny: []\n"

func m5BootstrapCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/captain/agent.md":    {Data: []byte("---\nname: captain\n---\n\nLead.\n")},
		"catalog/captain/policy.yaml": {Data: []byte(legacyCaptainPolicy)},
	})
}

func runM5LifecycleCommand(t *testing.T, cat *catalog.Catalog, args ...string) error {
	t.Helper()
	cmd := &cli.Command{Name: "shipmates", Commands: []*cli.Command{
		Init(cat), Add(cat), Update(cat), Remove(), Policy(),
	}}
	return cmd.Run(context.Background(), append([]string{"shipmates"}, args...))
}

func installManagedLegacyCaptainPolicy(t *testing.T) {
	t.Helper()
	path := project.PolicyPath("captain")
	legacy := []byte(legacyCaptainPolicy)
	if err := os.WriteFile(path, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := project.LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	m.Files[path] = project.SHA(legacy)
	if err := m.Save(); err != nil {
		t.Fatal(err)
	}
}

func TestM5LifecycleCommandsRepairManagedInvalidLegacyPolicy(t *testing.T) {
	t.Chdir(t.TempDir())
	cat := m5BootstrapCatalog()
	if err := runM5LifecycleCommand(t, cat, "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := runM5LifecycleCommand(t, cat, "add", "captain"); err != nil {
		t.Fatalf("initial add: %v", err)
	}

	installManagedLegacyCaptainPolicy(t)
	if err := runM5LifecycleCommand(t, cat, "update", "captain", "--accept", "theirs"); err != nil {
		t.Fatalf("update must be able to repair an invalid managed policy: %v", err)
	}
	assertStrictCaptainPolicy(t)

	installManagedLegacyCaptainPolicy(t)
	if err := runM5LifecycleCommand(t, cat, "add", "captain"); err != nil {
		t.Fatalf("add must be able to repair an invalid managed policy: %v", err)
	}
	assertStrictCaptainPolicy(t)
}

func TestM5LifecycleCommandsPreserveModifiedInvalidLegacyPolicy(t *testing.T) {
	t.Chdir(t.TempDir())
	cat := m5BootstrapCatalog()
	if err := runM5LifecycleCommand(t, cat, "init"); err != nil {
		t.Fatal(err)
	}
	if err := runM5LifecycleCommand(t, cat, "add", "captain"); err != nil {
		t.Fatal(err)
	}
	installManagedLegacyCaptainPolicy(t)

	path := project.PolicyPath("captain")
	modified := []byte(legacyCaptainPolicy + "# operator change\n")
	if err := os.WriteFile(path, modified, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runM5LifecycleCommand(t, cat, "add", "captain"); err != nil {
		t.Fatalf("add with modified invalid policy: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(modified) {
		t.Fatalf("add changed modified policy: %q", got)
	}

	if err := runM5LifecycleCommand(t, cat, "update", "captain", "--accept", "ours"); err != nil {
		t.Fatalf("update --accept ours with modified invalid policy: %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(modified) {
		t.Fatalf("update --accept ours changed modified policy: %q", got)
	}
}

func TestM5ExplicitValidationAndRuntimeTurnKeepTheirPolicyBoundaries(t *testing.T) {
	t.Chdir(t.TempDir())
	cat := m5BootstrapCatalog()
	if err := runM5LifecycleCommand(t, cat, "init"); err != nil {
		t.Fatal(err)
	}
	if err := runM5LifecycleCommand(t, cat, "add", "captain"); err != nil {
		t.Fatal(err)
	}
	installManagedLegacyCaptainPolicy(t)

	var out bytes.Buffer
	validate := &cli.Command{Name: "shipmates", Writer: &out, Commands: []*cli.Command{Policy()}}
	err := validate.Run(context.Background(), []string{"shipmates", "policy", "validate", "captain"})
	if err == nil || err.Error() != "policy validation failed" {
		t.Fatalf("policy validate error = %v", err)
	}
	if !strings.Contains(out.String(), `"valid":false`) || !strings.Contains(out.String(), `"diagnostics"`) {
		t.Fatalf("policy validate did not emit bounded diagnostics: %q", out.String())
	}

	// A turn command must still fail closed on the same invalid policy. The
	// failure happens before Codex lookup/start, so no model work can begin.
	ask := &cli.Command{Name: "shipmates", Commands: []*cli.Command{Ask()}}
	err = ask.Run(context.Background(), []string{"shipmates", "ask", "captain", "hello"})
	if err == nil || !strings.Contains(err.Error(), "captain is reserved") {
		t.Fatalf("ask error = %v, want reserved human captain", err)
	}
}

func TestM5InitAndRemoveCanRunWhileManagedPolicyIsInvalid(t *testing.T) {
	t.Chdir(t.TempDir())
	cat := m5BootstrapCatalog()
	if err := runM5LifecycleCommand(t, cat, "init"); err != nil {
		t.Fatal(err)
	}
	if err := runM5LifecycleCommand(t, cat, "add", "captain"); err != nil {
		t.Fatal(err)
	}
	installManagedLegacyCaptainPolicy(t)

	if err := runM5LifecycleCommand(t, cat, "init"); err != nil {
		t.Fatalf("init must bypass runtime policy preflight: %v", err)
	}
	if err := runM5LifecycleCommand(t, cat, "remove", "captain"); err != nil {
		t.Fatalf("remove must bypass runtime policy preflight: %v", err)
	}
	for _, path := range []string{project.CodexAgentPath("captain"), project.PolicyPath("captain")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("remove left %s: %v", path, err)
		}
	}
}

func assertStrictCaptainPolicy(t *testing.T) {
	t.Helper()
	got, err := os.ReadFile(project.PolicyPath("captain"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != emptyStrictPolicy {
		t.Fatalf("captain policy = %q, want strict M5 v1", got)
	}
	if info, err := os.Stat(filepath.Join(project.Dir, "policy.yaml")); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("base policy missing after lifecycle repair: %v", err)
	}
	if _, err := os.Stat(".legacy-runtime"); !os.IsNotExist(err) {
		t.Fatalf("lifecycle repair created legacy LegacyRuntime artifacts: %v", err)
	}
}
