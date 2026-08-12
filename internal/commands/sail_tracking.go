package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/luthermonson/shipmates/internal/beads"
	"github.com/luthermonson/shipmates/internal/tracker"
	"github.com/luthermonson/shipmates/internal/voyage"
)

// selectVoyageTracker picks the task-tracking backend for a voyage.
//
// Selection is automatic with an explicit override (shipmates.yaml
// voyage.tracker: markdown|beads). Auto picks beads only when bd is installed
// AND the workspace is initialized; everything else runs on the markdown
// backend — a first-class backend, not a degraded mode, so the notice is one
// INFO line. Explicitly configuring beads without bd is an error naming what
// to install, never a silent fallback.
func selectVoyageTracker(root, configured string, out io.Writer) (tracker.Tracker, error) {
	switch strings.TrimSpace(configured) {
	case "", "auto":
		if _, err := beads.Available(); err == nil && beads.Workspace(root) {
			return tracker.NewBeads(root)
		}
		fmt.Fprintf(out, "TRACKER  markdown (%s/) — install bd and run `shipmates beads init` to use the Beads backend\n", tracker.MarkdownDirName)
		return tracker.NewMarkdown(root), nil
	case "markdown":
		return tracker.NewMarkdown(root), nil
	case "beads":
		if _, err := beads.Available(); err != nil {
			return nil, errors.New("shipmates.yaml sets voyage.tracker: beads, but the bd CLI is not installed; install bd (https://github.com/gastownhall/beads) or set voyage.tracker: markdown")
		}
		if !beads.Workspace(root) {
			return nil, errors.New("shipmates.yaml sets voyage.tracker: beads, but this project has no initialized Beads workspace; run: shipmates beads init")
		}
		return tracker.NewBeads(root)
	default:
		return nil, fmt.Errorf("shipmates.yaml voyage.tracker %q is not a tracker (want markdown or beads)", configured)
	}
}

// sailTracking mirrors the voyage DAG into the selected tracking backend. The
// recorded id lives in voyage state's bead_id field regardless of backend.
type sailTracking struct {
	backend tracker.Tracker
	prime   string
	records map[string]string
}

// ensureDerivativeTask creates only newly commissioned successor work. Existing
// and inherited task records are never reopened, reassigned, or recreated.
func (b *sailTracking) ensureDerivativeTask(ctx context.Context, task voyage.Task, hash string, state *voyage.State, statePath string) error {
	if b == nil || b.backend == nil || state.Tasks[task.ID].BeadID != "" {
		return nil
	}
	description := fmt.Sprintf("Shipmates voyage derivative: %s\nPersona: %s\n\n%s", task.Summary, task.Persona, task.Prompt)
	id, err := b.backend.CreateTask(ctx, tracker.Task{Title: task.Summary, Description: description, Assignee: task.Persona, ExternalRef: "shipmates:derivative:" + hash[:16] + ":" + task.ID, Labels: []string{"shipmates", "voyage", "derivative", task.Persona}})
	if err != nil {
		return err
	}
	entry := state.Tasks[task.ID]
	entry.BeadID = id
	state.Tasks[task.ID] = entry
	for _, dependency := range task.DependsOn {
		dependencyState := state.Tasks[dependency]
		if dependencyState.BeadID == "" {
			continue
		}
		if err := b.backend.AddDependency(ctx, id, dependencyState.BeadID); err != nil {
			if dependencyState.Inherited != nil {
				continue // See prepareSailTracking: inherited edges are provenance.
			}
			return err
		}
	}
	entry.BeadDependenciesLinked = true
	state.Tasks[task.ID] = entry
	return voyage.SaveState(statePath, state)
}

func prepareSailTracking(ctx context.Context, backend tracker.Tracker, plan *voyage.Plan, state *voyage.State, hash, statePath string) (*sailTracking, error) {
	if backend == nil {
		return nil, nil
	}
	for _, task := range plan.Tasks {
		entry := state.Tasks[task.ID]
		if entry.Inherited != nil {
			// The predecessor record is immutable. Inherited prerequisites are
			// satisfied from their recorded state and are never recreated.
			continue
		}
		if entry.BeadID != "" {
			continue
		}
		description := fmt.Sprintf("Shipmates voyage: %s\nObjective: %s\nPersona: %s\n\n%s", plan.Title, plan.Objective, task.Persona, task.Prompt)
		id, err := backend.CreateTask(ctx, tracker.Task{
			Title:       task.Summary,
			Description: description,
			Assignee:    task.Persona,
			ExternalRef: "shipmates:voyage:" + hash[:16] + ":" + task.ID,
			Labels:      []string{"shipmates", "voyage", task.Persona},
		})
		if err != nil {
			return nil, fmt.Errorf("create %s task for %q: %w", backend.Name(), task.ID, err)
		}
		entry.BeadID = id
		state.Tasks[task.ID] = entry
		if err := voyage.SaveState(statePath, state); err != nil {
			return nil, err
		}
	}
	for _, task := range plan.Tasks {
		entry := state.Tasks[task.ID]
		if entry.Inherited != nil || entry.BeadID == "" {
			continue
		}
		if entry.BeadDependenciesLinked {
			continue
		}
		for _, dependency := range task.DependsOn {
			dependencyState := state.Tasks[dependency]
			if dependencyState.BeadID == "" {
				continue
			}
			if err := backend.AddDependency(ctx, entry.BeadID, dependencyState.BeadID); err != nil {
				// An inherited prerequisite's record belongs to the predecessor
				// voyage. Shipmates never creates, reopens, or relinks it, and it
				// may not be resolvable here at all — a predecessor state carried
				// in from another checkout, or `bd prune`/`bd gc` having reclaimed
				// the closed Bead. Real bd rejects `bd dep add <id> <missing-id>`,
				// so treating that edge as a dispatch prerequisite would refuse
				// the whole successor voyage over lost provenance. The edge is
				// recorded when the predecessor record is present and skipped when
				// it is not; the pending tasks' own records are still mandatory.
				if dependencyState.Inherited != nil {
					continue
				}
				return nil, fmt.Errorf("link %s dependency for task %q: %w", backend.Name(), task.ID, err)
			}
		}
		entry.BeadDependenciesLinked = true
		state.Tasks[task.ID] = entry
		if err := voyage.SaveState(statePath, state); err != nil {
			return nil, err
		}
	}
	graph := &sailTracking{backend: backend, prime: backend.Prime(ctx), records: make(map[string]string, len(plan.Tasks))}
	for _, task := range plan.Tasks {
		entry := state.Tasks[task.ID]
		graph.records[task.ID] = backend.Show(ctx, entry.BeadID)
	}
	return graph, nil
}

func (b *sailTracking) prompt(base string, task voyage.Task, entry voyage.TaskState) string {
	if b == nil || b.backend == nil || entry.BeadID == "" {
		return base
	}
	var context strings.Builder
	context.WriteString(base)
	context.WriteString("\n\nVOYAGE TASK TRACKER\n")
	context.WriteString(b.backend.TaskGuidance(entry.BeadID))
	if b.prime != "" {
		context.WriteString("\n\nProject tracker guidance:\n")
		context.WriteString(boundedSailText(b.prime, 12<<10))
	}
	if record := b.records[task.ID]; record != "" {
		context.WriteString("\n\nCurrent task record:\n")
		context.WriteString(boundedSailText(record, 8<<10))
	}
	return context.String()
}

func (b *sailTracking) start(ctx context.Context, task voyage.Task, entry voyage.TaskState) error {
	if b == nil || b.backend == nil || entry.BeadID == "" {
		return nil
	}
	return b.backend.Start(ctx, entry.BeadID, task.Persona)
}

func (b *sailTracking) finish(ctx context.Context, entry voyage.TaskState) error {
	if b == nil || b.backend == nil || entry.BeadID == "" {
		return nil
	}
	switch entry.Status {
	case voyage.Completed:
		return b.backend.Complete(ctx, entry.BeadID, entry.Summary)
	case voyage.Failed, voyage.Blocked, voyage.NeedsInput:
		reason := entry.Error
		if reason == "" {
			reason = string(entry.Status)
		}
		return b.backend.Block(ctx, entry.BeadID, reason)
	default:
		return nil
	}
}
