//go:build unix

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/luthermonson/shipmates/internal/beads/bdtest"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/voyage"
)

// These tests are the joint coverage that was missing entirely: Sail's real
// scheduler running end to end over a live Beads workspace backed by the real
// external bd CLI. Only the crew dispatcher is substituted, so no agent, model,
// or network turn is involved; everything between runSail and bd is genuine.
//
// They are unix-only because runSail itself refuses to run on Windows (see the
// GOOS guard at the top of runSail); the platform-independent half of the Beads
// mirroring is covered on Windows by sail_beads_live_test.go.

// TestSailVoyageMirrorsLifecycleIntoRealBeads runs a two-task voyage to
// completion and then asks bd what happened.
func TestSailVoyageMirrorsLifecycleIntoRealBeads(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "live beads voyage", Objective: "prove sail and bd together", Approved: true, Tasks: []voyage.Task{
		{ID: "design", Persona: "backend", Summary: "Design", Prompt: "produce the design"},
		{ID: "verify", Persona: "tester", Summary: "Verify", Prompt: "verify the design", DependsOn: []string{"design"}},
	}})
	bd := bdtest.Binary(t)
	bdtest.OnPath(t, bd)
	bdtest.InitAt(t, bd, root)
	t.Chdir(root)

	var mu sync.Mutex
	prompts := map[string]string{}
	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	sailTaskDispatcher = func(_ context.Context, persona *project.InstalledPersona, prompt string, _ project.PersonaConfig, stdout, _ io.Writer) error {
		mu.Lock()
		prompts[persona.Name] = prompt
		mu.Unlock()
		_, _ = fmt.Fprintf(stdout, "%s finished the job\n", persona.Name)
		return nil
	}

	var output bytes.Buffer
	if err := runSailCommand(context.Background(), &output); err != nil {
		t.Fatalf("sail failed: %v\n%s", err, output.String())
	}
	if !strings.Contains(output.String(), "DONE 2") {
		t.Fatalf("sail output:\n%s", output.String())
	}

	// Sail created exactly one bead per task and closed both.
	ids := bdtest.OpenIDs(t, bd, root)
	if len(ids) != 2 {
		t.Fatalf("bd holds %d beads, want 2: %v", len(ids), ids)
	}
	byTitle := map[string]map[string]any{}
	for _, id := range ids {
		record := bdtest.Issue(t, bd, root, id)
		byTitle[bdtest.Field(t, record, "title")] = record
	}
	for title, persona := range map[string]string{"Design": "backend", "Verify": "tester"} {
		record, ok := byTitle[title]
		if !ok {
			t.Fatalf("bd has no bead titled %q; got %v", title, byTitle)
		}
		if got := bdtest.Field(t, record, "status"); got != "closed" {
			t.Errorf("%s bead status = %q, want closed", title, got)
		}
		if got := bdtest.Field(t, record, "assignee"); got != persona {
			t.Errorf("%s bead assignee = %q, want %q", title, got, persona)
		}
		if got := bdtest.Field(t, record, "close_reason"); got != "Shipmates voyage task completed" {
			t.Errorf("%s bead close_reason = %q", title, got)
		}
		if got := bdtest.Comments(t, bd, root, bdtest.Field(t, record, "id")); !strings.Contains(got, persona+" finished the job") {
			t.Errorf("%s bead is missing the crew report:\n%s", title, got)
		}
	}
	verifyID := bdtest.Field(t, byTitle["Verify"], "id")
	designID := bdtest.Field(t, byTitle["Design"], "id")
	if !bdtest.DependsOn(t, bd, root, verifyID, designID) {
		t.Errorf("bd does not record the voyage DAG edge %s -> %s", verifyID, designID)
	}

	// The crew prompt carried the real bead record and real bd prime guidance.
	mu.Lock()
	testerPrompt := prompts["tester"]
	mu.Unlock()
	for _, want := range []string{"BEADS TASK GRAPH", "Bead: " + verifyID, "bd show " + verifyID + " --json", "Beads Workflow Context", "Current Bead record:"} {
		if !strings.Contains(testerPrompt, want) {
			t.Fatalf("tester prompt missing %q:\n%s", want, testerPrompt)
		}
	}

	// Voyage state stores only the opaque ids bd handed back.
	statePath, beadIDs := persistedBeadIDs(t, root)
	if beadIDs["design"] != designID || beadIDs["verify"] != verifyID {
		t.Fatalf("voyage state %s recorded %v, want design=%s verify=%s", statePath, beadIDs, designID, verifyID)
	}
}

// persistedBeadIDs reads the single voyage state file sail wrote and returns the
// bead id recorded for each task.
func persistedBeadIDs(t *testing.T, root string) (string, map[string]string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, ".shipmates", "voyages", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("voyage state files = %v", matches)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var state struct {
		Tasks map[string]struct {
			BeadID string `json:"bead_id"`
		} `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatalf("decode %s: %v", matches[0], err)
	}
	out := make(map[string]string, len(state.Tasks))
	for id, entry := range state.Tasks {
		out[id] = entry.BeadID
	}
	return matches[0], out
}

// TestSailVoyageMarksFailedAndDependencyBlockedBeadsInRealBeads covers the
// non-happy path: a failing crew turn must leave its bead blocked in bd, and the
// dependent task must be blocked too rather than closed.
func TestSailVoyageMarksFailedAndDependencyBlockedBeadsInRealBeads(t *testing.T) {
	root := sailTestProject(t, voyage.Plan{Version: 1, Title: "blocked beads voyage", Objective: "prove terminal sync", Approved: true, Tasks: []voyage.Task{
		{ID: "design", Persona: "backend", Summary: "Design", Prompt: "produce the design"},
		{ID: "verify", Persona: "tester", Summary: "Verify", Prompt: "verify the design", DependsOn: []string{"design"}},
	}})
	bd := bdtest.Binary(t)
	bdtest.OnPath(t, bd)
	bdtest.InitAt(t, bd, root)
	t.Chdir(root)

	old := sailTaskDispatcher
	defer func() { sailTaskDispatcher = old }()
	sailTaskDispatcher = func(_ context.Context, persona *project.InstalledPersona, _ string, _ project.PersonaConfig, _, _ io.Writer) error {
		if persona.Name == "backend" {
			return errors.New("the design could not be produced")
		}
		return errors.New("tester should never have been dispatched")
	}

	var output bytes.Buffer
	err := runSailCommand(context.Background(), &output)
	if err == nil {
		t.Fatalf("a failing voyage reported success:\n%s", output.String())
	}
	if !strings.Contains(err.Error(), "voyage incomplete") {
		t.Fatalf("sail error = %v\n%s", err, output.String())
	}

	statuses := map[string]string{}
	for _, id := range bdtest.OpenIDs(t, bd, root) {
		record := bdtest.Issue(t, bd, root, id)
		statuses[bdtest.Field(t, record, "title")] = bdtest.Field(t, record, "status")
	}
	if statuses["Design"] != "blocked" {
		t.Errorf("failed task bead status = %q, want blocked", statuses["Design"])
	}
	if statuses["Verify"] != "blocked" {
		t.Errorf("dependency-blocked task bead status = %q, want blocked", statuses["Verify"])
	}
	if got := bdtest.Comments(t, bd, root, beadIDForTitle(t, bd, root, "Design")); !strings.Contains(got, "the design could not be produced") {
		t.Errorf("failed bead is missing its reason:\n%s", got)
	}
}

func beadIDForTitle(t *testing.T, bd, root, title string) string {
	t.Helper()
	for _, id := range bdtest.OpenIDs(t, bd, root) {
		if bdtest.Field(t, bdtest.Issue(t, bd, root, id), "title") == title {
			return id
		}
	}
	t.Fatalf("bd has no bead titled %q", title)
	return ""
}
