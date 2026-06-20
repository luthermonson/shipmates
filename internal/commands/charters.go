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

// installedPersonas lists persona names present in .claude/agents/.
func installedPersonas() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(project.AgentsDir, "*.md"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range matches {
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

// Autonomous prints a lead scheduler charter to feed into a scheduler (cron,
// Claude Code CronCreate, GitHub Actions, systemd timer, …). Shipmates stays
// harness-neutral: it renders the prompt; you wire the schedule.
func Autonomous(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "autonomous",
		Usage: "print a lead scheduler charter to feed into your scheduler",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "print-charter", Usage: "print the scheduler charter (currently the only mode)"},
			&cli.StringFlag{Name: "persona", Value: "lead", Usage: "the lead/scheduler persona"},
			&cli.StringFlag{Name: "cadence", Value: "5min,10,15,20,30", Usage: "backoff cadence ladder"},
			&cli.IntFlag{Name: "cap", Value: 3, Usage: "max drain per persona per cycle"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if !c.Bool("print-charter") {
				return errors.New("only --print-charter is supported — pipe its output into your scheduler")
			}
			lead := c.String("persona")

			all, err := installedPersonas()
			if err != nil {
				return err
			}
			var crew []string
			for _, p := range all {
				if p != lead {
					crew = append(crew, p)
				}
			}

			charter, err := renderCharter(cat, "autonomous", map[string]any{
				"Lead":        lead,
				"CrewList":    strings.Join(crew, ", "),
				"Cap":         c.Int("cap"),
				"Cadence":     c.String("cadence"),
				"RoutingRead": routingStateRead(),
			})
			if err != nil {
				return err
			}
			fmt.Print(charter)
			return nil
		},
	}
}
