package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
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

func installedPersonas() ([]string, error) {
	return project.InstalledPersonas()
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
			installed, err := project.CanonicalPersonaAt(".", persona)
			if err != nil {
				return err
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
			return dispatchToInstalled(ctx, installed, charter, c.Bool("fresh"), os.Stdout, os.Stderr)
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
			inventory, err := project.CanonicalPersonaInventory(".")
			if err != nil {
				return err
			}
			byName := make(map[string]*project.InstalledPersona, len(inventory))
			for i := range inventory {
				byName[inventory[i].Name] = &inventory[i]
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

					installed := byName[persona]
					if installed == nil {
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
					err = dispatchToInstalled(ctx, installed, charter, false, &buf, &buf)
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

// Autonomous prints a bounded Codex-native orchestration charter. Shipmates
// does not install or manage a scheduler; callers may run the rendered cycle
// from an external scheduler if desired.
func Autonomous(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "autonomous",
		Usage: "print a bounded Codex-native orchestration charter",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "persona", Value: "skipper", Usage: "execution coordinator persona"},
			&cli.StringFlag{Name: "cadence", Value: "5min,10,15,20,30", Usage: "external scheduler cadence hint"},
			&cli.IntFlag{Name: "cap", Value: 3, Usage: "maximum tasks per persona in one cycle"},
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

func autonomousCharter(cat *catalog.Catalog, coordinator, cadence string, cap int) (string, error) {
	cfg, err := project.LoadConfig()
	if err != nil {
		return "", err
	}
	skipper := strings.TrimSpace(cfg.SkipperPersona)
	if skipper == "" {
		skipper = "skipper"
	}
	if coordinator != skipper {
		return "", fmt.Errorf("autonomous coordinator must be configured skipper %q", skipper)
	}
	if _, err := project.CanonicalPersonaAt(".", skipper); err != nil {
		return "", fmt.Errorf("configured skipper %q is not installed: %w", skipper, err)
	}
	all, err := installedPersonas()
	if err != nil {
		return "", err
	}
	var crew []string
	for _, persona := range all {
		if persona != coordinator {
			crew = append(crew, persona)
		}
	}
	return renderCharter(cat, "autonomous", map[string]any{
		"Coordinator": coordinator,
		"CrewList":    strings.Join(crew, ", "),
		"Cap":         cap,
		"Cadence":     cadence,
		"RoutingRead": routingStateRead(),
	})
}
