package tracker

import (
	"context"
	"strings"
	"testing"
)

// contractBackends returns every backend the shared contract suite runs
// against. Markdown runs everywhere; the beads backend is added on unix by
// beads_contract_test.go through a `#!/bin/sh` stand-in for bd (the tests
// stop at the argv boundary and never exec the real bd, which boots an
// embedded Dolt engine).
type contractBackend struct {
	name string
	make func(t *testing.T) Tracker
}

var extraContractBackends []contractBackend

func contractBackendsForTest() []contractBackend {
	backends := []contractBackend{{
		name: "markdown",
		make: func(t *testing.T) Tracker { return NewMarkdown(t.TempDir()) },
	}}
	return append(backends, extraContractBackends...)
}

// TestTrackerContract is the one suite both backends must pass: the exact
// operations sail consumes, asserted only on behavior both backends honestly
// provide.
func TestTrackerContract(t *testing.T) {
	for _, backend := range contractBackendsForTest() {
		t.Run(backend.name, func(t *testing.T) {
			tr := backend.make(t)
			ctx := context.Background()
			if tr.Name() == "" {
				t.Fatal("backend has no name")
			}
			parent, err := tr.CreateTask(ctx, Task{
				Title:       "Design the adapter",
				Description: "Shipmates voyage: prove the tracker contract\nPersona: architect",
				Assignee:    "architect",
				ExternalRef: "shipmates:voyage:0123456789abcdef:design",
				Labels:      []string{"shipmates", "voyage", "architect"},
			})
			if err != nil || parent == "" {
				t.Fatalf("CreateTask parent = %q, %v", parent, err)
			}
			child, err := tr.CreateTask(ctx, Task{
				Title:       "Build on the design",
				Assignee:    "backend",
				ExternalRef: "shipmates:voyage:0123456789abcdef:build",
				Labels:      []string{"shipmates", "voyage", "backend"},
			})
			if err != nil || child == "" {
				t.Fatalf("CreateTask child = %q, %v", child, err)
			}
			if child == parent {
				t.Fatalf("backend reused task id %q", child)
			}
			if err := tr.AddDependency(ctx, child, parent); err != nil {
				t.Fatalf("AddDependency: %v", err)
			}
			if err := tr.Start(ctx, parent, "architect"); err != nil {
				t.Fatalf("Start: %v", err)
			}
			if err := tr.Complete(ctx, parent, "verified against the tracker contract"); err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if err := tr.Block(ctx, child, "waiting on the captain"); err != nil {
				t.Fatalf("Block: %v", err)
			}
			if shown := tr.Show(ctx, parent); !strings.Contains(shown, parent) {
				t.Fatalf("Show(%s) = %q, want the record to name the task", parent, shown)
			}
			if guidance := tr.TaskGuidance(child); !strings.Contains(guidance, child) {
				t.Fatalf("TaskGuidance(%s) = %q, want the instruction block to name the task", child, guidance)
			}
			// Prompt context is best-effort by design: an unknown id yields "".
			if got := tr.Show(ctx, "does-not-exist"); got != "" {
				t.Fatalf("Show(unknown) = %q, want empty", got)
			}
		})
	}
}
