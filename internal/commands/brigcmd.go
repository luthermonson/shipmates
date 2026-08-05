package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/urfave/cli/v3"
)

// Brig exposes the Ship's Articles subsystem — show the operator's posture,
// list the rules, explain one, and inspect the denial log. There is no
// `brig install`: on this codebase the kernel Articles compile straight into
// the permissions evaluator at server start, and the prompt reminder is
// spliced by persona install/update, so being installed IS being bound.
func Brig() *cli.Command {
	return &cli.Command{
		Name:  "brig",
		Usage: "the Ship's Articles — security & hardening rules bound to every persona",
		Commands: []*cli.Command{
			brigStatusCommand(),
			brigListCommand(),
			brigExplainCommand(),
			brigLogCommand(),
		},
	}
}

// Freeze creates the .shipmates/freeze marker file, engaging the emergency
// stop: the captain's PreToolUse gate refuses every Write-class tool call —
// bypass-mode personas included — until `shipmates release` clears it.
func Freeze() *cli.Command {
	return &cli.Command{
		Name:  "freeze",
		Usage: "engage the Brig freeze — refuse all Write operations until released",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "reason", Usage: "one-line reason recorded in the marker"},
			&cli.StringFlag{Name: "admiral", Usage: "who invoked the freeze (defaults to $USER)"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			settings := brig.Load("")
			if settings.Disabled("respect-the-freeze") {
				// Refusing (rather than writing a marker nothing enforces) is
				// the honest answer: an emergency stop that doesn't stop
				// anything would be worse than no stop at all.
				if !settings.Enabled {
					return errors.New("the brig is disabled in ~/.shipmates/config.yaml (brig.enabled: false); the freeze is not enforced while it is off")
				}
				return errors.New("Article 12 (respect-the-freeze) is waived in ~/.shipmates/config.yaml; the freeze is not enforced while it is waived")
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
			if err := brig.SetFreeze(".", reason, admiral); err != nil {
				return err
			}
			fmt.Fprintf(c.Writer, "Brig freeze engaged (reason: %s, admiral: %s).\n", reason, admiral)
			fmt.Fprintf(c.Writer, "Marker: %s\n", brig.FreezeMarkerPath("."))
			fmt.Fprintln(c.Writer, "Release with: shipmates release")
			return nil
		},
	}
}

// Release removes the .shipmates/freeze marker, releasing the emergency stop.
// Missing marker is not an error — release is idempotent. Release works even
// with the brig disabled: clearing operator state must never be gated on the
// posture that made the state moot.
func Release() *cli.Command {
	return &cli.Command{
		Name:  "release",
		Usage: "release the Brig freeze — allow Write operations to resume",
		Action: func(ctx context.Context, c *cli.Command) error {
			frozen, marker := brig.CheckFreeze(".")
			if err := brig.ClearFreeze("."); err != nil {
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

func brigStatusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "show the operator's brig posture and the freeze state",
		Action: func(ctx context.Context, c *cli.Command) error {
			settings := brig.Load("")
			if settings.Enabled {
				fmt.Fprintln(c.Writer, "brig: enabled")
			} else {
				fmt.Fprintln(c.Writer, "brig: DISABLED in ~/.shipmates/config.yaml — no Articles are enforced (the fleet-wide deny list still is)")
			}
			if waived := settings.DisabledHandles(); len(waived) > 0 {
				fmt.Fprintf(c.Writer, "waived articles: %s\n", strings.Join(waived, ", "))
			}
			frozen, marker := brig.CheckFreeze(".")
			switch {
			case !frozen:
				fmt.Fprintln(c.Writer, "freeze: not engaged")
			case settings.Disabled("respect-the-freeze"):
				fmt.Fprintln(c.Writer, "freeze: marker present but NOT enforced (brig disabled or Article 12 waived); it resumes effect on re-enable, or `shipmates release` clears it")
			case marker != nil:
				fmt.Fprintf(c.Writer, "freeze: ENGAGED (reason: %s, admiral: %s, since: %s)\n",
					marker.Reason, marker.Admiral, marker.Timestamp.Format("2006-01-02T15:04:05Z"))
			default:
				fmt.Fprintln(c.Writer, "freeze: ENGAGED (marker unreadable — fail-closed)")
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
			return writeBrigList(c.Writer, rules, brig.Load(""))
		},
	}
}

func writeBrigList(w io.Writer, rules []brig.Rule, settings brig.Settings) error {
	for _, r := range rules {
		status := ""
		if settings.Disabled(r.Handle) {
			status = "  (waived)"
		}
		if _, err := fmt.Fprintf(w, "%2d  %-26s  %-14s  %s%s\n", r.Number, r.Handle, layerLabels(r.Layers), r.Title, status); err != nil {
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
			entries, err := brig.ReadDenials(".")
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
