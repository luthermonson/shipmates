package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

func Routing(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{Name: "routing", Usage: "manage the project routing block on persona files", Commands: []*cli.Command{
		{Name: "apply", Usage: "compose the active routing block into Codex persona(s)", ArgsUsage: "<persona>...", Flags: []cli.Flag{
			&cli.BoolFlag{Name: "all", Usage: "apply to every installed Codex persona"},
		}, Action: func(ctx context.Context, c *cli.Command) error {
			conf, err := project.LoadConfig()
			if err != nil {
				return err
			}
			if conf.Routing == "" {
				return errors.New("no routing declared in shipmates.yaml (set e.g. `routing: github`)")
			}
			names := c.Args().Slice()
			if c.Bool("all") {
				if len(names) != 0 {
					return errors.New("routing apply --all does not accept persona arguments")
				}
			}
			if len(names) == 0 && !c.Bool("all") {
				return errors.New("usage: shipmates routing apply <persona>... (or --all)")
			}
			return withPolicyWriteLock(func() error { return applyCodexRoutingSelection(cat, names, c.Bool("all"), conf.Routing) })
		}},
		{Name: "show", Usage: "print the active routing block (per routing: + routingOptions:)", Action: func(ctx context.Context, c *cli.Command) error {
			block, err := renderRoutingBlock(cat)
			if err != nil {
				return err
			}
			if block == nil {
				return errors.New("no routing declared in shipmates.yaml (set e.g. `routing: github`)")
			}
			_, err = os.Stdout.Write(block)
			return err
		}},
	}}
}

func applyCodexRouting(cat *catalog.Catalog, names []string, routing string) error {
	return applyCodexRoutingSelection(cat, names, false, routing)
}

func applyCodexRoutingSelection(cat *catalog.Catalog, names []string, all bool, routing string) error {
	if !project.RoutingTransactionsSupported() {
		return errors.New("atomic routing transactions are unsupported on this platform")
	}
	if err := project.RecoverRoutingTransaction("."); err != nil {
		return err
	}
	seen := map[string]bool{}
	m, manifestHash, err := project.LoadManifestSnapshot(".")
	if err != nil {
		return err
	}
	if m.Version != project.ManifestVersion {
		return fmt.Errorf("routing apply requires manifest version %s", project.ManifestVersion)
	}
	inventory, err := project.CanonicalPersonaInventory(".")
	if err != nil {
		return err
	}
	byName := make(map[string]project.InstalledPersona, len(inventory))
	for _, item := range inventory {
		byName[item.Name] = item
	}
	if all {
		names = names[:0]
		for _, item := range inventory {
			names = append(names, item.Name)
		}
	}
	if len(names) == 0 {
		return errors.New("no installed Codex personas")
	}
	next := &project.Manifest{Version: m.Version, Files: make(map[string]string, len(m.Files))}
	for k, v := range m.Files {
		next.Files[k] = v
	}
	artifacts := make([]project.RoutingArtifact, 0, len(names))
	for _, name := range names {
		if seen[name] {
			return fmt.Errorf("duplicate persona %q", name)
		}
		seen[name] = true
		item, ok := byName[name]
		if !ok {
			return fmt.Errorf("persona %q is not installed — run: shipmates add %s", name, name)
		}
		rel := project.CodexAgentPath(name)
		baseline, tracked := m.Files[rel]
		if !tracked {
			return fmt.Errorf("persona %q Codex artifact is not managed by the manifest", name)
		}
		if project.SHA(item.Raw) != baseline {
			return fmt.Errorf("persona %q Codex artifact has local modifications", name)
		}
		composed, err := applyRouting(cat, []byte(item.Doc.Instructions))
		if err != nil {
			return err
		}
		out, err := item.Doc.ReplaceInstructions(strings.TrimSpace(string(composed)))
		if err != nil {
			return fmt.Errorf("persona %q: %w", name, err)
		}
		artifacts = append(artifacts, project.RoutingArtifact{Name: name, OldHash: baseline, OldMode: item.Mode, OldDevice: item.Device, OldInode: item.Inode, Output: out})
		next.Files[rel] = project.SHA(out)
	}
	manifest, err := project.SerializeManifestV2(next)
	if err != nil {
		return err
	}
	if err := project.ApplyRoutingTransaction(".", artifacts, manifestHash, manifest); err != nil {
		return err
	}
	for _, a := range artifacts {
		slog.Info("applied routing block", "persona", a.Name, "routing", routing)
	}
	return nil
}
