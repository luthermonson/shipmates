package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Brig exposes the Ship's Articles subsystem — list rules, explain a rule,
// inspect the denial log, install the kernel-policy overlay into every
// installed persona, and (with --fleet) write the fleet-wide baseline.
func Brig(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "brig",
		Usage: "the Ship's Articles — security & hardening rules bound to every persona",
		Commands: []*cli.Command{
			brigListCommand(),
			brigExplainCommand(),
			brigLogCommand(),
			brigInstallCommand(cat),
		},
	}
}

// Freeze creates the .shipmates/freeze marker file, engaging the emergency
// stop. Every persona bound by Article 12 refuses Write and Edit operations
// until `shipmates release` clears the marker.
func Freeze() *cli.Command {
	return &cli.Command{
		Name:  "freeze",
		Usage: "engage the Brig freeze — refuse all Write operations until released",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "reason", Usage: "one-line reason recorded in the marker"},
			&cli.StringFlag{Name: "admiral", Usage: "who invoked the freeze (defaults to $USER)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			root, err := project.FindRoot(".")
			if err != nil {
				return err
			}
			reason := strings.TrimSpace(c.String("reason"))
			if reason == "" {
				reason = "no reason recorded"
			}
			admiral := strings.TrimSpace(c.String("admiral"))
			if admiral == "" {
				admiral = os.Getenv("USER")
				if admiral == "" {
					admiral = os.Getenv("USERNAME") // Windows
				}
			}
			if err := brig.SetFreeze(root, reason, admiral); err != nil {
				return err
			}
			fmt.Fprintf(c.Writer, "Brig freeze engaged (reason: %s, admiral: %s).\n", reason, admiral)
			fmt.Fprintf(c.Writer, "Marker: %s\n", brig.FreezeMarkerPath(root))
			fmt.Fprintln(c.Writer, "Release with: shipmates release")
			return nil
		},
	}
}

// Release removes the .shipmates/freeze marker, releasing the emergency stop.
// Missing marker is not an error — release is idempotent.
func Release() *cli.Command {
	return &cli.Command{
		Name:  "release",
		Usage: "release the Brig freeze — allow Write operations to resume",
		Action: func(ctx context.Context, c *cli.Command) error {
			root, err := project.FindRoot(".")
			if err != nil {
				return err
			}
			frozen, marker := brig.CheckFreeze(root)
			if err := brig.ClearFreeze(root); err != nil {
				return err
			}
			if !frozen {
				fmt.Fprintln(c.Writer, "No freeze in effect.")
				return nil
			}
			if marker != nil {
				fmt.Fprintf(c.Writer, "Released Brig freeze (was: %s / %s / %s).\n",
					marker.Reason, marker.Admiral, marker.Timestamp.Format("2006-01-02T15:04:05Z"))
			} else {
				fmt.Fprintln(c.Writer, "Released Brig freeze.")
			}
			return nil
		},
	}
}

func brigListCommand() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list all fifteen Articles with handle and one-line title",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "code", Usage: "list only Articles of Code (1-5)"},
			&cli.BoolFlag{Name: "conduct", Usage: "list only Articles of Conduct (6-15)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			rules := brig.Rules()
			if c.Bool("code") {
				rules = brig.RulesForCategory(brig.CategoryCode)
			} else if c.Bool("conduct") {
				rules = brig.RulesForCategory(brig.CategoryConduct)
			}
			return writeBrigList(c.Writer, rules)
		},
	}
}

func writeBrigList(w io.Writer, rules []brig.Rule) error {
	for _, r := range rules {
		layers := layerLabels(r.Layers)
		if _, err := fmt.Fprintf(w, "%2d  %-24s  %-9s  %s\n", r.Number, r.Handle, layers, r.Title); err != nil {
			return err
		}
	}
	return nil
}

func layerLabels(ls []brig.Layer) string {
	out := make([]string, 0, len(ls))
	for _, l := range ls {
		out = append(out, string(l))
	}
	return strings.Join(out, "+")
}

func brigExplainCommand() *cli.Command {
	return &cli.Command{
		Name:      "explain",
		Usage:     "print the full rule text with rationale, source, and enforcement layer",
		ArgsUsage: "<N>",
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 1 {
				return errors.New("usage: shipmates brig explain <N>")
			}
			raw := c.Args().First()
			n, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("expected a rule number (1..15), got %q", raw)
			}
			rule, err := brig.Get(n)
			if err != nil {
				return err
			}
			return writeBrigExplain(c.Writer, rule)
		},
	}
}

func writeBrigExplain(w io.Writer, r brig.Rule) error {
	fmt.Fprintf(w, "Article %d — %s\n", r.Number, r.Title)
	fmt.Fprintf(w, "Handle:   %s\n", r.Handle)
	fmt.Fprintf(w, "Category: %s\n", r.Category)
	fmt.Fprintf(w, "Layers:   %s\n", layerLabels(r.Layers))
	if r.Source != "" {
		fmt.Fprintf(w, "Source:   %s\n", r.Source)
	}
	fmt.Fprintf(w, "\n%s\n", r.Rationale)
	return nil
}

func brigLogCommand() *cli.Command {
	return &cli.Command{
		Name:  "log",
		Usage: "print entries from .shipmates/brig.log",
		Flags: []cli.Flag{
			&cli.IntFlag{Name: "tail", Value: 0, Usage: "show only the last N entries (0 = all)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			root, err := project.FindRoot(".")
			if err != nil {
				return err
			}
			entries, err := brig.ReadDenials(root)
			if err != nil {
				return err
			}
			if tail := int(c.Int("tail")); tail > 0 && len(entries) > tail {
				entries = entries[len(entries)-tail:]
			}
			if len(entries) == 0 {
				fmt.Fprintln(c.Writer, "(no denials logged)")
				return nil
			}
			for _, e := range entries {
				fmt.Fprintf(c.Writer, "%s  Article %-2d  %-16s  %s\n",
					e.Timestamp.Format("2006-01-02T15:04:05Z"), e.Rule, e.Persona, e.Command)
			}
			return nil
		},
	}
}

func brigInstallCommand(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "merge the Brig kernel-policy template into every installed persona overlay",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "dry-run", Usage: "print what would be written; do not modify files"},
			&cli.BoolFlag{Name: "fleet", Usage: "write catalog/fleet-brig.default.yaml to ~/.shipmates/brig.yaml if missing"},
			&cli.BoolFlag{Name: "code-scanners", Usage: "install semgrep + owasp-dependency-check pre-commit hooks (RESERVED — prints follow-up notice)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Bool("code-scanners") {
				fmt.Fprintln(c.Writer,
					"shipmates brig install --code-scanners is reserved for a follow-up PR.\n"+
						"See docs/brig.md § Enforcement notes for what it will wire up.")
			}
			if c.Bool("fleet") {
				return brigInstallFleet(c.Writer, cat, c.Bool("dry-run"))
			}
			return brigInstallProject(c.Writer, cat, c.Bool("dry-run"))
		},
	}
}

func brigInstallProject(w io.Writer, cat *catalog.Catalog, dryRun bool) error {
	template, err := cat.BrigPolicyTemplate()
	if err != nil {
		return fmt.Errorf("read Brig template: %w", err)
	}
	root, err := project.FindRoot(".")
	if err != nil {
		return err
	}
	installed, err := project.InstalledPersonasAt(root)
	if err != nil {
		return err
	}
	if len(installed) == 0 {
		fmt.Fprintln(w, "No personas are installed in this project. Run `shipmates add <persona>` first.")
		return nil
	}
	for _, name := range installed {
		path := filepath.Join(root, project.PolicyPath(name))
		if dryRun {
			fmt.Fprintf(w, "would merge Brig template into %s\n", path)
			continue
		}
		if err := brig.MergeInto(path, template); err != nil {
			return fmt.Errorf("merge into %s: %w", path, err)
		}
		fmt.Fprintf(w, "merged Brig template into %s\n", path)
	}
	return nil
}

func brigInstallFleet(w io.Writer, cat *catalog.Catalog, dryRun bool) error {
	body, err := cat.FleetBrigDefault()
	if err != nil {
		return fmt.Errorf("read fleet Brig default: %w", err)
	}
	target, err := fleetBrigPath()
	if err != nil {
		return err
	}
	if dryRun {
		fmt.Fprintf(w, "would write %d bytes to %s (if missing)\n", len(body), target)
		return nil
	}
	if _, err := os.Stat(target); err == nil {
		fmt.Fprintf(w, "fleet Brig already exists at %s; leaving untouched\n", target)
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(target, body, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote fleet Brig defaults to %s\n", target)
	return nil
}

// fleetBrigPath returns ~/.shipmates/brig.yaml with $HOME resolved. Falls back
// to a project-local .shipmates/brig.yaml when the user has no home dir
// (rare, but shipmates has run under those conditions in CI).
func fleetBrigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".shipmates", "brig.yaml"), nil
	}
	return filepath.Join(home, ".shipmates", "brig.yaml"), nil
}
