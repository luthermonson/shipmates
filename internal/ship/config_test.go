package ship

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestConfigPath(t *testing.T) {
	got, err := ConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".shipmates", "ship.yaml")
	if got != want {
		t.Fatalf("ConfigPath = %q, want %q", got, want)
	}
}

func TestLoadConfigMissingFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("want error for a missing config file")
	}
}

func TestLoadConfigMalformedYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ship.yaml")
	if err := os.WriteFile(path, []byte("projects:\n  - dir: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("want a parse error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("error = %q, want it to name the offending file", err)
	}
}

func TestLoadConfigNoProjectsKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ship.yaml")
	if err := os.WriteFile(path, []byte("env:\n  FOO: bar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("want error when the config lists no projects at all")
	}
}

func TestLoadConfigFileInsteadOfDir(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ship.yaml")
	if err := os.WriteFile(path, []byte("projects:\n  - dir: "+filepath.ToSlash(notADir)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A regular file is not a supervisable project dir; it must be dropped,
	// which leaves nothing usable.
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("want error: a plain file is not a project dir")
	}
}

func TestLoadConfigKeepsEnvAndTrimsDirs(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ship.yaml")
	body := "env:\n  HOST: one\nprojects:\n  - dir: \"  " + filepath.ToSlash(proj) + "  \"\n    env:\n      PROJ: two\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Env["HOST"] != "one" {
		t.Fatalf("host env = %v", c.Env)
	}
	if len(c.Projects) != 1 {
		t.Fatalf("projects = %+v", c.Projects)
	}
	// The whitespace-trimmed dir must be what's kept — an untrimmed path fails
	// every later os.Stat and exec.Cmd.Dir.
	if c.Projects[0].Dir != filepath.ToSlash(proj) {
		t.Fatalf("dir = %q, want the trimmed %q", c.Projects[0].Dir, filepath.ToSlash(proj))
	}
	if c.Projects[0].Env["PROJ"] != "two" {
		t.Fatalf("project env = %v", c.Projects[0].Env)
	}
}

func TestEnvStartsFromProcessEnvironment(t *testing.T) {
	t.Setenv("SHIP_TEST_INHERITED", "yes")
	env := (&Config{}).env(Project{Dir: "."})
	if !slices.Contains(env, "SHIP_TEST_INHERITED=yes") {
		t.Fatal("env must inherit the supervisor's own environment")
	}
}

func TestEnvExpandsUnsetVarToEmpty(t *testing.T) {
	c := &Config{Env: map[string]string{"TOKEN": "${SHIP_TEST_DEFINITELY_UNSET_VAR}"}}
	env := c.env(Project{Dir: "."})
	if !slices.Contains(env, "TOKEN=") {
		t.Fatalf("unset reference should expand to empty, got %v", lastMatching(env, "TOKEN="))
	}
}

func lastMatching(env []string, prefix string) []string {
	var out []string
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			out = append(out, e)
		}
	}
	return out
}

func TestAddProjectCreatesFile(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cfg", "ship.yaml") // parent dir does not exist yet
	if err := AddProject(path, proj); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	var c Config
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("AddProject did not create %s: %v", path, err)
	}
	if err := yaml.Unmarshal(b, &c); err != nil {
		t.Fatalf("AddProject wrote unparseable yaml: %v\n%s", err, b)
	}
	if len(c.Projects) != 1 {
		t.Fatalf("projects = %+v", c.Projects)
	}
	// Stored absolute and slash-normalised so the same dir is recognised no
	// matter which separator or relative form the user typed.
	want := filepath.ToSlash(proj)
	if c.Projects[0].Dir != want {
		t.Fatalf("dir = %q, want %q", c.Projects[0].Dir, want)
	}
}

func TestAddProjectResolvesRelativePath(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	path := filepath.Join(dir, "ship.yaml")
	if err := AddProject(path, "repo"); err != nil {
		t.Fatalf("AddProject: %v", err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.Projects[0].Dir) {
		t.Fatalf("dir = %q, want an absolute path (the supervisor runs from elsewhere)", c.Projects[0].Dir)
	}
}

func TestAddProjectAppends(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{a, b} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "ship.yaml")
	if err := AddProject(path, a); err != nil {
		t.Fatal(err)
	}
	if err := AddProject(path, b); err != nil {
		t.Fatal(err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 2 {
		t.Fatalf("projects = %+v, want both dirs kept", c.Projects)
	}
}

func TestAddProjectRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ship.yaml")
	if err := AddProject(path, proj); err != nil {
		t.Fatal(err)
	}
	err := AddProject(path, proj)
	if err == nil {
		t.Fatal("want error re-adding the same dir — two captains would fight over one port file")
	}
	if !strings.Contains(err.Error(), "already in") {
		t.Fatalf("error = %q", err)
	}
	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Projects) != 1 {
		t.Fatalf("a rejected add must not modify the file: %+v", c.Projects)
	}
}

func TestAddProjectRejectsDuplicateSpelledDifferently(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ship.yaml")
	if err := AddProject(path, proj); err != nil {
		t.Fatal(err)
	}
	// Same dir reached via "." indirection and native separators: normalisation
	// (Abs + ToSlash) must collapse it to the stored form.
	alias := filepath.Join(dir, ".", "repo")
	if err := AddProject(path, alias); err == nil {
		t.Fatalf("want duplicate error for %q, which resolves to the stored dir", alias)
	}
}

func TestAddProjectRejectsNonDirectory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ship.yaml")

	missing := filepath.Join(dir, "nope")
	if err := AddProject(path, missing); err == nil {
		t.Fatal("want error for a missing dir")
	}

	file := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := AddProject(path, file)
	if err == nil {
		t.Fatal("want error for a plain file")
	}
	if !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("error = %q", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("a rejected add must not create the config file")
	}
}

func TestAddProjectMalformedExistingConfig(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ship.yaml")
	if err := os.WriteFile(path, []byte("projects: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := AddProject(path, proj)
	if err == nil {
		t.Fatal("want a parse error rather than silently clobbering the user's file")
	}
	b, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(b) != "projects: [unclosed\n" {
		t.Fatalf("the unparseable file was overwritten: %q", b)
	}
}
