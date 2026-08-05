//go:build unix

// This test drives a `#!/bin/sh` stand-in for bd so it can assert the exact
// argv the sail/Beads bridge builds — something the real CLI cannot show. It
// is not evidence about bd's behavior: the stand-in exits 0 for every
// subcommand. The external contract is verified against the real binary by
// the opt-in tests in sail_beads_live_test.go and internal/beads/live_test.go.
package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/tracker"
	"github.com/luthermonson/shipmates/internal/voyage"
)

func TestPrepareSailTrackingMirrorsVoyageGraphIntoBeadsArgv(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir(".beads", 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(bin, "bd.log")
	script := filepath.Join(bin, "bd")
	body := `#!/bin/sh
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
printf '%s\n' "$*" >> "$root/bd.log"
case "$1" in
create)
  n=0
  test -f "$root/count" && n=$(cat "$root/count")
  n=$((n+1))
  printf '%s' "$n" > "$root/count"
  printf '{"id":"ship-%s"}\n' "$n"
  ;;
prime) printf 'Use bd ready and bd show before working.\n' ;;
show) printf '[{"id":"%s","status":"open"}]\n' "$2" ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	backend, err := tracker.NewBeads(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := &voyage.Plan{
		Version: 1, Title: "Beads voyage", Objective: "Prove graph mirroring", Commissioned: true,
		Tasks: []voyage.Task{
			{ID: "design", Persona: "architect", Summary: "Design", Prompt: "Produce design", DependsOn: []string{}},
			{ID: "build", Persona: "backend", Summary: "Build", Prompt: "Implement design", DependsOn: []string{"design"}},
		},
	}
	state := voyage.NewState(plan, strings.Repeat("a", 64))
	statePath := filepath.Join(root, ".shipmates", "voyages", "state.json")
	graph, err := prepareSailTracking(context.Background(), backend, plan, state, strings.Repeat("a", 64), statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.Tasks["design"].BeadID != "ship-1" || state.Tasks["build"].BeadID != "ship-2" {
		t.Fatalf("bead ids = %+v", state.Tasks)
	}
	if !state.Tasks["build"].BeadDependenciesLinked {
		t.Fatal("dependency link was not persisted")
	}
	prompt := graph.prompt("base", plan.Tasks[1], state.Tasks["build"])
	for _, want := range []string{"Bead: ship-2", "bd show ship-2 --json", "Use bd ready", `"status":"open"`} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt missing %q:\n%s", want, prompt)
		}
	}
	entry := state.Tasks["build"]
	if err := graph.start(context.Background(), plan.Tasks[1], entry); err != nil {
		t.Fatal(err)
	}
	entry.Status, entry.Summary = voyage.Completed, "verified"
	if err := graph.finish(context.Background(), entry); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{
		"dep add ship-2 ship-1",
		"update ship-2 --status=in_progress --assignee=backend",
		// --author precedes the "--" end-of-options sentinel guarding the
		// persona-influenced summary (OWASP audit fix in beads/client.go).
		"comments add ship-2 --author=shipmates -- verified",
		"close ship-2 --reason=Shipmates voyage task completed",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("bd log missing %q:\n%s", want, log)
		}
	}
}

func TestPrepareSailTrackingDoesNotDuplicateInheritedPrerequisite(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	if err := os.Mkdir(".beads", 0o700); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "bd.log")
	script := filepath.Join(bin, "bd")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + logPath + "\"\ncase \"$1\" in create) printf '%s\\n' '{\"id\":\"successor-bead\"}' ;; prime) ;; show) printf '%s\\n' '[]' ;; esac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	backend, err := tracker.NewBeads(root)
	if err != nil {
		t.Fatal(err)
	}
	plan := &voyage.Plan{Version: 1, Title: "successor", Objective: "inherit safely", Commissioned: true, Tasks: []voyage.Task{
		{ID: "one", Persona: "backend", Summary: "One", Prompt: "one"},
		{ID: "two", Persona: "tester", Summary: "Two", Prompt: "two", DependsOn: []string{"one"}},
	}}
	state := voyage.NewState(plan, strings.Repeat("b", 64))
	entry := state.Tasks["one"]
	entry.Status, entry.BeadID = voyage.Completed, "Shipmates-auz"
	entry.Inherited = &voyage.InheritedTask{PredecessorPlanHash: strings.Repeat("c", 64), PredecessorTaskID: "one", TaskFingerprint: strings.Repeat("d", 64), ClosureFingerprint: strings.Repeat("e", 64), Summary: "original", FinishedAt: state.StartedAt, OriginalBeadID: "Shipmates-auz"}
	state.Tasks["one"] = entry
	statePath := filepath.Join(root, ".shipmates", "voyages", "state.json")
	if _, err := prepareSailTracking(context.Background(), backend, plan, state, strings.Repeat("b", 64), statePath); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log), "create --json --type=task --title=One") || !strings.Contains(string(log), "create --json --type=task --title=Two") {
		t.Fatalf("inherited Bead was duplicated or pending Bead missing: %s", log)
	}
}
