package ship

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestLoadConfigFiltersMissingDirs(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ship.yaml")
	yaml := "projects:\n  - dir: " + proj + "\n  - dir: " + filepath.Join(dir, "gone") + "\n  - dir: \"\"\n"
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 1 || c.Projects[0].Dir != proj {
		t.Fatalf("want just %s, got %+v", proj, c.Projects)
	}
}

func TestLoadConfigNoUsableProjects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ship.yaml")
	if err := os.WriteFile(path, []byte("projects: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("want error for empty project list")
	}
}

func TestEnvIndirection(t *testing.T) {
	t.Setenv("SHIP_TEST_SECRET", "s3cret")
	c := &Config{
		Env: map[string]string{"HOST_LEVEL": "on", "SHIPMATES_BRIDGE_TOKEN": "${SHIP_TEST_SECRET}"},
	}
	p := Project{Dir: ".", Env: map[string]string{"HOST_LEVEL": "overridden"}}
	env := c.env(p)
	for _, want := range []string{"SHIPMATES_BRIDGE_TOKEN=s3cret", "HOST_LEVEL=overridden"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q", want)
		}
	}
	// per-project entries come after host-level, so the override wins for
	// consumers that take the last occurrence (os/exec does)
	if slices.Index(env, "HOST_LEVEL=on") > slices.Index(env, "HOST_LEVEL=overridden") {
		t.Error("per-project env must come after host-level env")
	}
}
