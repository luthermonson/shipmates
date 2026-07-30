package commands

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/luthermonson/shipmates/internal/berth"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Berth is the operator-facing surface for per-persona worktrees. Berths are
// created and reused automatically at spawn time for any persona configured
// with `crew.<persona>.berth: auto|require` in shipmates.yaml; this command
// exists so an operator can provision, inspect and tear one down without
// waiting for a session — the "launch ergonomics" half of
// docs/persona-berths.md.
//
// It deliberately does not spawn anything. `shipmates berth ensure <persona>`
// prints the resolved path so it composes with a shell (`cd "$(shipmates
// berth ensure skipper)"`).
func Berth() *cli.Command {
	return &cli.Command{
		Name:  "berth",
		Usage: "manage per-persona git worktrees (.shipmates/berths/<persona>)",
		Commands: []*cli.Command{
			berthEnsure(),
			berthPath(),
			berthList(),
			berthRemove(),
		},
	}
}

// berthEnsure provisions (or reuses) a persona's berth and prints its path.
// The berth policy is honored: a persona configured `off` produces no berth
// and prints nothing, so a caller can branch on empty output.
func berthEnsure() *cli.Command {
	return &cli.Command{
		Name:      "ensure",
		Usage:     "create or reuse a persona's berth, printing the resolved path",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "policy", Usage: "override the configured policy for this call: off|auto|require"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name, root, cfg, err := berthTarget(c)
			if err != nil {
				return err
			}
			policy := berth.ParsePolicy(cfg.Berth)
			if raw := c.String("policy"); raw != "" {
				policy = berth.ParsePolicy(raw)
			}
			path, err := berth.EnsureAt(root, name, policy)
			if err != nil {
				return err
			}
			if path == "" {
				fmt.Fprintf(c.Root().ErrWriter, "persona %q has no berth (policy %s); it runs at the project root\n", name, policy)
				return nil
			}
			fmt.Fprintln(c.Root().Writer, path)
			return nil
		},
	}
}

// berthPath reports where a persona's session will actually run, without
// creating anything. This is the resolution the spawn sites perform, so it is
// the honest answer to "where does this mate work?" — always an absolute path,
// matching what `ensure` prints, so the two are interchangeable in a script.
func berthPath() *cli.Command {
	return &cli.Command{
		Name:      "path",
		Usage:     "print where a persona's session will run, creating nothing",
		ArgsUsage: "<persona>",
		Action: func(ctx context.Context, c *cli.Command) error {
			name, root, cfg, err := berthTarget(c)
			if err != nil {
				return err
			}
			target := root
			switch {
			case cfg.CWD != "":
				target = cfg.CWD
				if !filepath.IsAbs(target) {
					target = filepath.Join(root, target)
				}
			case berth.ParsePolicy(cfg.Berth) != berth.PolicyOff:
				target = filepath.Join(root, berth.Dir(name))
			}
			fmt.Fprintln(c.Root().Writer, target)
			return nil
		},
	}
}

// berthList reports the berths git currently has registered for this repo.
// Reading git rather than the filesystem means a directory somebody parked
// under .shipmates/berths/ is never reported as a berth.
func berthList() *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list personas that currently have a berth",
		Action: func(ctx context.Context, c *cli.Command) error {
			root, err := project.CanonicalRoot(".")
			if err != nil {
				return err
			}
			for _, name := range berth.List(root) {
				dirty, _ := berth.IsDirty(berth.Dir(name))
				state := "clean"
				if dirty {
					state = "dirty"
				}
				fmt.Fprintf(c.Root().Writer, "%-14s %-8s %s\n", name, state, berth.Dir(name))
			}
			return nil
		},
	}
}

// berthRemove tears a berth down on its own, without removing the persona.
// Same refusals as `shipmates remove`: dirty needs --force, a nested
// per-issue worktree refuses either way.
func berthRemove() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "tear down a persona's berth (keeps the persona installed)",
		ArgsUsage: "<persona>",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "force", Usage: "remove the berth even if it has uncommitted changes"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			name := c.Args().First()
			if name == "" {
				return errors.New("usage: shipmates berth remove <persona>")
			}
			if err := project.ValidatePersonaName(name); err != nil {
				return err
			}
			root, err := project.CanonicalRoot(".")
			if err != nil {
				return err
			}
			return berth.RemoveAt(root, name, c.Bool("force"))
		},
	}
}

// berthTarget validates the persona argument and resolves both the project
// root and the persona's launch configuration. Personas do not have to be
// installed to have a berth provisioned — a berth is a directory, not an
// artifact — but the name still has to be a legal persona name, because it
// becomes a path segment and a git branch name.
func berthTarget(c *cli.Command) (name, root string, cfg project.PersonaConfig, err error) {
	name = c.Args().First()
	if name == "" {
		return "", "", cfg, fmt.Errorf("usage: shipmates berth %s <persona>", c.Name)
	}
	if err := project.ValidatePersonaName(name); err != nil {
		return "", "", cfg, err
	}
	root, err = project.CanonicalRoot(".")
	if err != nil {
		return "", "", cfg, err
	}
	cfg, err = project.ResolvePersonaConfigAt(root, name)
	if err != nil {
		return "", "", cfg, err
	}
	return name, root, cfg, nil
}
