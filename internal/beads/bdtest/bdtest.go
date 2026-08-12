// Package bdtest provisions the real external bd CLI for the Beads
// integration tests and verifies what bd actually reports back.
//
// The internal/beads adapter parses bd's stdout (see beads.issueID), so the
// only honest verification of that contract is to run the real binary. This
// package is imported exclusively from _test.go files; nothing in the shipmates
// command tree links it.
//
// Provisioning is deliberately not done by this package: tests never download
// anything. bd is discovered on PATH (or named explicitly by SHIPMATES_TEST_BD)
// so an ordinary `go test` stays offline and fast. CI installs the pinned
// release listed in Version and sets SHIPMATES_TEST_BD_REQUIRED=1, which turns
// a missing bd from a skip into a failure — the real-bd tests are the only
// coverage of the external contract and must not vanish quietly.
package bdtest

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

const (
	// Version is the bd release whose command surface and JSON output the
	// internal/beads adapter targets and these tests verify. Bump it together
	// with the pinned download in .github/workflows/test.yml and the Beads
	// integration section of docs/sailing.md.
	Version = "1.1.2"

	// BinaryEnv names an explicit bd executable and bypasses PATH lookup.
	BinaryEnv = "SHIPMATES_TEST_BD"

	// RequiredEnv makes a missing bd a hard failure instead of a skip.
	RequiredEnv = "SHIPMATES_TEST_BD_REQUIRED"

	// Prefix is the issue prefix given to throwaway test workspaces.
	Prefix = "ship"
)

// runTimeout bounds every helper invocation. bd embeds a Dolt engine and takes
// roughly a second to start a workspace, so this is generous on purpose; it
// exists to fail a wedged command rather than to measure performance.
const runTimeout = 3 * time.Minute

// Binary resolves the real external bd CLI. The live tests are explicit
// opt-in: they run only when BinaryEnv names an executable or RequiredEnv is
// set to 1 (CI). A bd that merely sits on PATH does not trigger them — every
// bd invocation spins up an embedded Dolt engine, and a developer's ordinary
// `go test ./...` must never launch that just because bd is installed.
func Binary(t testing.TB) string {
	t.Helper()
	if explicit := strings.TrimSpace(os.Getenv(BinaryEnv)); explicit != "" {
		info, err := os.Stat(explicit)
		if err != nil {
			t.Fatalf("%s=%q is not usable: %v", BinaryEnv, explicit, err)
		}
		if !info.Mode().IsRegular() {
			t.Fatalf("%s=%q is not a regular file", BinaryEnv, explicit)
		}
		return explicit
	}
	if os.Getenv(RequiredEnv) != "1" {
		t.Skipf("real-bd contract tests are opt-in: set %s=1 (CI) or point %s at a bd %s executable", RequiredEnv, BinaryEnv, Version)
		return ""
	}
	command, lookErr := exec.LookPath("bd")
	if lookErr != nil {
		t.Fatalf("real bd CLI is required (%s=1) but was not found on PATH: %v\ninstall bd %s or point %s at the executable", RequiredEnv, lookErr, Version, BinaryEnv)
	}
	return command
}

// OnPath makes the resolved bd discoverable as plain "bd" for the duration of
// the test, so code paths that go through beads.Available/exec.LookPath — not
// just an injected Client.Command — are exercised too.
func OnPath(t testing.TB, bd string) {
	t.Helper()
	dir := filepath.Dir(bd)
	if filepath.Base(bd) != commandName() {
		dir = t.TempDir()
		link(t, bd, filepath.Join(dir, commandName()))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved, err := exec.LookPath("bd")
	if err != nil {
		t.Fatalf("bd is not resolvable on the test PATH: %v", err)
	}
	if !strings.EqualFold(filepath.Clean(resolved), filepath.Clean(filepath.Join(dir, commandName()))) {
		t.Fatalf("bd resolved to %q, want %q", resolved, filepath.Join(dir, commandName()))
	}
}

// Isolate points bd's and git's configuration lookups at a throwaway home so
// tests never read or mutate the developer's real bd config, memories, or
// global Beads state. It returns the temporary home.
func Isolate(t testing.TB) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	if runtime.GOOS == "windows" {
		t.Setenv("USERPROFILE", home)
		t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming"))
		t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	}
	return home
}

// Workspace resolves bd, creates a throwaway directory, and initializes a real
// Beads workspace in it. It returns the bd path and the workspace root.
func Workspace(t testing.TB) (string, string) {
	t.Helper()
	bd := Binary(t)
	// beads.New resolves symlinks before comparing roots; hand back the same
	// resolved form so callers can compare paths without surprises.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	InitAt(t, bd, root)
	return bd, root
}

// InitAt initializes a real Beads workspace inside an existing directory.
//
// bd init creates a git repository and commits its own files, so the directory
// is given a deterministic repository-local git identity first; otherwise the
// result would depend on the developer's or runner's global git configuration.
func InitAt(t testing.TB, bd, root string) {
	t.Helper()
	Isolate(t)
	if _, err := exec.LookPath("git"); err != nil {
		if os.Getenv(RequiredEnv) == "1" {
			t.Fatalf("git is required to initialize a Beads workspace: %v", err)
		}
		t.Skipf("git is not installed; bd init needs it to create its repository: %v", err)
	}
	git(t, root, "init", "--quiet")
	for _, setting := range [][2]string{
		{"user.email", "shipmates-test@example.invalid"},
		{"user.name", "Shipmates Test"},
		{"commit.gpgsign", "false"},
		// bd 1.1.2 warns on stderr when this is unset (upstream GH#2950).
		{"beads.role", "maintainer"},
	} {
		git(t, root, "config", setting[0], setting[1])
	}
	// bd reports anonymous usage metrics to a network endpoint by default.
	// Keep the test suite off the wire; the isolated home makes this local.
	if _, err := run(t, bd, root, "metrics", "off"); err != nil {
		t.Logf("bd metrics off failed (continuing): %v", err)
	}
	Run(t, bd, root, "init", "--non-interactive", "--skip-agents", "--skip-hooks", "--prefix", Prefix)
}

// Run executes bd in root and returns its stdout, failing the test on error.
func Run(t testing.TB, bd, root string, args ...string) string {
	t.Helper()
	out, err := run(t, bd, root, args...)
	if err != nil {
		t.Fatalf("bd %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// Issue returns the record bd reports for id.
//
// bd 1.1.2 emits a JSON *array* from `bd show <id> --json` even for a single
// id; asserting that here keeps the shape documented and detects drift.
func Issue(t testing.TB, bd, root, id string) map[string]any {
	t.Helper()
	raw := Run(t, bd, root, "show", id, "--json")
	var list []map[string]any
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("bd show %s --json did not decode as a JSON array (%v):\n%s", id, err, raw)
	}
	if len(list) != 1 {
		t.Fatalf("bd show %s --json returned %d records:\n%s", id, len(list), raw)
	}
	return list[0]
}

// Field returns a record field as a string, failing when it is absent.
func Field(t testing.TB, issue map[string]any, name string) string {
	t.Helper()
	value, ok := issue[name]
	if !ok {
		t.Fatalf("bd issue record has no %q field: %v", name, keys(issue))
	}
	text, ok := value.(string)
	if !ok {
		t.Fatalf("bd issue field %q is %T, want string", name, value)
	}
	return text
}

// OpenIDs returns every issue id bd currently knows about, in bd's own order.
func OpenIDs(t testing.TB, bd, root string) []string {
	t.Helper()
	raw := Run(t, bd, root, "list", "--all", "--json")
	var list []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatalf("bd list --all --json did not decode (%v):\n%s", err, raw)
	}
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, item.ID)
	}
	return out
}

// Comments returns the rendered comment thread for an issue.
func Comments(t testing.TB, bd, root, id string) string {
	t.Helper()
	return Run(t, bd, root, "comments", id)
}

// DependsOn reports whether bd records issue as depending on dependency.
func DependsOn(t testing.TB, bd, root, issue, dependency string) bool {
	t.Helper()
	return strings.Contains(Run(t, bd, root, "dep", "list", issue), dependency)
}

func run(t testing.TB, bd, root string, args ...string) (string, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bd, args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "BD_NON_INTERACTIVE=1", "CI=true")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		return stdout.String(), fmt.Errorf("%w (stderr: %s)", err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

func git(t testing.TB, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func commandName() string {
	if runtime.GOOS == "windows" {
		return "bd.exe"
	}
	return "bd"
}

// link prefers a hard link so the 140+ MiB bd binary is not copied; a copy is
// the fallback when source and destination are on different volumes.
func link(t testing.TB, from, to string) {
	t.Helper()
	if err := os.Link(from, to); err == nil {
		return
	}
	source, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	destination, err := os.OpenFile(to, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(destination, source); err != nil {
		_ = destination.Close()
		t.Fatal(err)
	}
	if err := destination.Close(); err != nil {
		t.Fatal(err)
	}
}

func keys(record map[string]any) []string {
	out := make([]string, 0, len(record))
	for key := range record {
		out = append(out, key)
	}
	return out
}
