package commands

import (
	"context"
	"fmt"
	"strings"

	"github.com/luthermonson/shipmates/internal/beads"
	"github.com/luthermonson/shipmates/internal/voyage"
)

type sailBeads struct {
	client  *beads.Client
	prime   string
	records map[string]string
}

func prepareSailBeads(ctx context.Context, plan *voyage.Plan, state *voyage.State, hash, statePath string) (*sailBeads, error) {
	if !beads.Workspace(".") {
		return nil, nil
	}
	client, err := beads.New(".")
	if err != nil {
		return nil, err
	}
	for _, task := range plan.Tasks {
		entry := state.Tasks[task.ID]
		if entry.Inherited != nil {
			// The predecessor Bead is immutable. Inherited prerequisites are
			// satisfied from their recorded state and are never recreated.
			continue
		}
		if entry.BeadID != "" {
			continue
		}
		description := fmt.Sprintf("Shipmates voyage: %s\nObjective: %s\nPersona: %s\n\n%s", plan.Title, plan.Objective, task.Persona, task.Prompt)
		id, err := client.CreateTask(ctx, beads.Task{
			Title:       task.Summary,
			Description: description,
			Assignee:    task.Persona,
			ExternalRef: "shipmates:voyage:" + hash[:16] + ":" + task.ID,
			Labels:      []string{"shipmates", "voyage", task.Persona},
		})
		if err != nil {
			return nil, fmt.Errorf("create Bead for task %q: %w", task.ID, err)
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
			dependencyBead := state.Tasks[dependency].BeadID
			if dependencyBead == "" {
				continue
			}
			if err := client.AddDependency(ctx, entry.BeadID, dependencyBead); err != nil {
				return nil, fmt.Errorf("link Bead dependency for task %q: %w", task.ID, err)
			}
		}
		entry.BeadDependenciesLinked = true
		state.Tasks[task.ID] = entry
		if err := voyage.SaveState(statePath, state); err != nil {
			return nil, err
		}
	}
	graph := &sailBeads{client: client, prime: client.Prime(ctx), records: make(map[string]string, len(plan.Tasks))}
	for _, task := range plan.Tasks {
		entry := state.Tasks[task.ID]
		graph.records[task.ID] = client.Show(ctx, entry.BeadID)
	}
	return graph, nil
}

func (b *sailBeads) prompt(base string, task voyage.Task, entry voyage.TaskState) string {
	if b == nil || entry.BeadID == "" {
		return base
	}
	var context strings.Builder
	fmt.Fprintf(&context, "%s\n\nBEADS TASK GRAPH\nBead: %s\nUse `bd show %s --json` for the authoritative task record and `bd comments add %s <note>` for concise durable findings. Shipmates will synchronize terminal status; do not create a duplicate task.", base, entry.BeadID, entry.BeadID, entry.BeadID)
	if b.prime != "" {
		context.WriteString("\n\nProject Beads guidance:\n")
		context.WriteString(boundedSailText(b.prime, 12<<10))
	}
	if record := b.records[task.ID]; record != "" {
		context.WriteString("\n\nCurrent Bead record:\n")
		context.WriteString(boundedSailText(record, 8<<10))
	}
	return context.String()
}

func (b *sailBeads) start(ctx context.Context, task voyage.Task, entry voyage.TaskState) error {
	if b == nil || entry.BeadID == "" {
		return nil
	}
	return b.client.Start(ctx, entry.BeadID, task.Persona)
}

func (b *sailBeads) finish(ctx context.Context, entry voyage.TaskState) error {
	if b == nil || entry.BeadID == "" {
		return nil
	}
	switch entry.Status {
	case voyage.Completed:
		return b.client.Complete(ctx, entry.BeadID, entry.Summary)
	case voyage.Failed, voyage.Blocked, voyage.NeedsInput:
		reason := entry.Error
		if reason == "" {
			reason = string(entry.Status)
		}
		return b.client.Block(ctx, entry.BeadID, reason)
	default:
		return nil
	}
}
