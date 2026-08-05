package tracker

import (
	"context"
	"fmt"

	"github.com/luthermonson/shipmates/internal/beads"
)

// Beads adapts the bounded bd CLI client to the Tracker seam. Everything
// specific to bd — argv construction, the environment allowlist, output
// bounds — stays in internal/beads; this adapter only maps the vocabulary.
type Beads struct {
	Client *beads.Client
}

// NewBeads opens the Beads backend for an initialized workspace.
func NewBeads(root string) (*Beads, error) {
	client, err := beads.New(root)
	if err != nil {
		return nil, err
	}
	return &Beads{Client: client}, nil
}

func (b *Beads) Name() string { return "beads" }

func (b *Beads) CreateTask(ctx context.Context, t Task) (string, error) {
	return b.Client.CreateTask(ctx, beads.Task{Title: t.Title, Description: t.Description, Assignee: t.Assignee, ExternalRef: t.ExternalRef, Labels: t.Labels})
}

func (b *Beads) AddDependency(ctx context.Context, id, dependsOn string) error {
	return b.Client.AddDependency(ctx, id, dependsOn)
}

func (b *Beads) Start(ctx context.Context, id, persona string) error {
	return b.Client.Start(ctx, id, persona)
}

func (b *Beads) Complete(ctx context.Context, id, summary string) error {
	return b.Client.Complete(ctx, id, summary)
}

func (b *Beads) Block(ctx context.Context, id, reason string) error {
	return b.Client.Block(ctx, id, reason)
}

func (b *Beads) Show(ctx context.Context, id string) string { return b.Client.Show(ctx, id) }

func (b *Beads) Prime(ctx context.Context) string { return b.Client.Prime(ctx) }

func (b *Beads) TaskGuidance(id string) string {
	return fmt.Sprintf("Bead: %s\nUse `bd show %s --json` for the authoritative task record and `bd comments add %s <note>` for concise durable findings. Shipmates will synchronize terminal status; do not create a duplicate task.", id, id, id)
}
