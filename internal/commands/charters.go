package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"text/template"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// renderCharter loads a catalog charter template and renders it with data.
func renderCharter(cat *catalog.Catalog, name string, data any) (string, error) {
	raw, err := cat.CharterFile(name)
	if err != nil {
		return "", fmt.Errorf("read charter %q: %w", name, err)
	}
	tmpl, err := template.New(name).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse charter %q: %w", name, err)
	}
	var b bytes.Buffer
	if err := tmpl.Execute(&b, data); err != nil {
		return "", fmt.Errorf("render charter %q: %w", name, err)
	}
	return b.String(), nil
}

// activeRouting returns the configured routing layer, or "" on any config error.
func activeRouting() string {
	conf, err := project.LoadConfig()
	if err != nil || conf == nil {
		return ""
	}
	return conf.Routing
}

// routingFlow expands the project's routing layer into a one-line "how work
// flows" description for the drain charter.
func routingFlow() string {
	switch activeRouting() {
	case "github":
		return "claim the issue by comment → branch a worktree off origin/main → open a PR with `Closes #N` → land a peer `Verdict: LGTM` → merge --delete-branch → run the cleanup ceremony (worktree remove, branch -D, git pull origin main)"
	default:
		return "the project's standard claim → implement → review → merge flow"
	}
}

// routingStateRead expands the routing layer into a "read the queue" instruction
// for the scheduler charter.
func routingStateRead() string {
	switch activeRouting() {
	case "github":
		return "git fetch, then read the queue: `gh issue list --state open` and `gh pr list --state open` (classify per /standup)."
	default:
		return "read the project's work queue."
	}
}

// installedPersonas lists fleet persona names present in .claude/agents/,
// excluding files that opt out via `shipmatesPersona: false` (e.g. a non-fleet
// project-Q&A subagent).
func installedPersonas() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(project.AgentsDir, "*.md"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range matches {
		if !project.IsFleetPersonaFile(m) {
			continue
		}
		names = append(names, strings.TrimSuffix(filepath.Base(m), ".md"))
	}
	sort.Strings(names)
	return names, nil
}

// Drain dispatches a persona to drain its work queue (one priority pick per
// loop) until empty or capped, then exit. Composes the canonical drain charter
// with the project's routing layer.
func Drain(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "drain",
		Usage:     "dispatch a persona to drain its work queue, then exit",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "cap", Value: 5, Usage: "max issues to ship this session"},
			&cli.StringFlag{Name: "prompt", Usage: "extra context appended to the drain charter"},
			&cli.BoolFlag{Name: "fresh", Usage: "start a new session"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			persona := c.Args().First()
			if persona == "" {
				return errors.New("usage: shipmates drain <persona>")
			}
			if _, err := os.Stat(project.AgentPath(persona)); err != nil {
				return fmt.Errorf("persona %q is not installed", persona)
			}

			charter, err := renderCharter(cat, "drain", map[string]any{
				"Persona":     persona,
				"Cap":         c.Int("cap"),
				"RoutingFlow": routingFlow(),
			})
			if err != nil {
				return err
			}
			if extra := strings.TrimSpace(c.String("prompt")); extra != "" {
				charter += "\n\n" + extra
			}
			return dispatch(ctx, persona, charter, c.Bool("fresh"))
		},
	}
}

// DrainMany drains several personas' queues in parallel (bounded concurrency),
// each through the same drain charter as `drain`. Output is captured per persona
// and printed under headers once all finish.
func DrainMany(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "drain-many",
		Usage:     "drain several personas' queues in parallel",
		ArgsUsage: "<persona>...",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Usage: "drain every fleet persona"},
			&cli.IntFlag{Name: "cap", Value: 5, Usage: "per-persona issue cap"},
			&cli.IntFlag{Name: "max-concurrent", Value: 4, Usage: "max personas draining at once"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			personas := c.Args().Slice()
			if c.Bool("all") {
				p, err := installedPersonas()
				if err != nil {
					return err
				}
				personas = p
			}
			if len(personas) == 0 {
				return errors.New("usage: shipmates drain-many <persona>... (or --all)")
			}

			max := c.Int("max-concurrent")
			if max < 1 {
				max = len(personas)
			}
			sem := make(chan struct{}, max)
			results := make([]fanoutResult, len(personas))
			var wg sync.WaitGroup
			for i, persona := range personas {
				wg.Add(1)
				go func(i int, persona string) {
					defer wg.Done()
					sem <- struct{}{}
					defer func() { <-sem }()

					if _, err := os.Stat(project.AgentPath(persona)); err != nil {
						results[i] = fanoutResult{persona: persona, err: fmt.Errorf("persona %q is not installed", persona)}
						return
					}
					charter, err := renderCharter(cat, "drain", map[string]any{
						"Persona":     persona,
						"Cap":         c.Int("cap"),
						"RoutingFlow": routingFlow(),
					})
					if err != nil {
						results[i] = fanoutResult{persona: persona, err: err}
						return
					}
					var buf bytes.Buffer
					err = dispatchTo(ctx, persona, charter, false, &buf, &buf)
					results[i] = fanoutResult{persona: persona, output: buf.Bytes(), err: err}
				}(i, persona)
			}
			wg.Wait()

			failures := 0
			for _, r := range results {
				fmt.Printf("==== %s ====\n", r.persona)
				if len(r.output) > 0 {
					_, _ = os.Stdout.Write(r.output)
					if !bytes.HasSuffix(r.output, []byte("\n")) {
						fmt.Println()
					}
				}
				if r.err != nil {
					failures++
					fmt.Printf("error: %v\n", r.err)
				}
				fmt.Println()
			}
			if failures == len(results) {
				return fmt.Errorf("all %d drains failed", len(results))
			}
			return nil
		},
	}
}

// Autonomous renders the captain scheduler charter and prints it. Shipmates stays
// harness-neutral and can't wire the schedule itself: Claude Code's durable cron
// only persists when created from an interactive session (a headless `claude -p`
// cron is session-only and evaporates), so the actual scheduling is done by
// running the /autonomous slash command in an interactive captain session.
func Autonomous(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "autonomous",
		Usage: "print the captain scheduler charter (wire it via the /autonomous slash command)",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "print-charter", Usage: "print the charter to stdout"},
			&cli.StringFlag{Name: "persona", Value: "captain", Usage: "the captain/scheduler persona"},
			&cli.StringFlag{Name: "cadence", Value: "5min,10,15,20,30", Usage: "backoff cadence ladder"},
			&cli.IntFlag{Name: "cap", Value: 3, Usage: "max drain per persona per cycle"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			charter, err := autonomousCharter(cat, c.String("persona"), c.String("cadence"), c.Int("cap"))
			if err != nil {
				return err
			}
			fmt.Print(charter)
			return nil
		},
	}
}

// autonomousCharter renders the scheduler charter for the given captain, with the
// crew read from the installed fleet personas (excluding the captain).
func autonomousCharter(cat *catalog.Catalog, captain, cadence string, cap int) (string, error) {
	all, err := installedPersonas()
	if err != nil {
		return "", err
	}
	var crew []string
	for _, p := range all {
		if p != captain {
			crew = append(crew, p)
		}
	}
	return renderCharter(cat, "autonomous", map[string]any{
		"Captain":     captain,
		"CrewList":    strings.Join(crew, ", "),
		"Cap":         cap,
		"Cadence":     cadence,
		"RoutingRead": routingStateRead(),
	})
}

