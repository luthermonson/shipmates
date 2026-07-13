package commands

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

type removeOps struct {
	mkdirTemp func(string, string) (string, error)
	stage     func(string, string) error
	restore   func(string, string) error
	remove    func(string) error
	save      func(*project.Manifest) error
}

var personaRemoveOps = removeOps{
	mkdirTemp: os.MkdirTemp,
	stage:     os.Rename, restore: os.Rename, remove: os.Remove,
	save: func(m *project.Manifest) error { return m.Save() },
}

type stagedRemoval struct{ target, staged, dir string }

// Remove removes clean tracked Codex persona artifacts, keeping memory unless --purge.
func Remove() *cli.Command {
	return &cli.Command{
		Name:      "remove",
		Usage:     "remove a Codex persona's managed artifacts (keeps memory unless --purge)",
		ArgsUsage: "<persona>",
		Flags:     []cli.Flag{&cli.BoolFlag{Name: "purge", Usage: "also delete the persona's memory dir"}},
		Action: func(ctx context.Context, c *cli.Command) error {
			name := c.Args().First()
			if name == "" {
				return errors.New("usage: shipmates remove <persona>")
			}

			return runRemove(name, c.Bool("purge"))
		},
	}
}

func runRemove(name string, purge bool) error {
	if err := project.ValidatePersonaName(name); err != nil {
		return err
	}
	m, err := project.LoadManifest()
	if err != nil {
		return err
	}
	if err := requireManifestV2(m, "remove"); err != nil {
		return err
	}
	return withPolicyWriteLock(func() error { return runRemoveLocked(name, purge, m) })
}

func runRemoveLocked(name string, purge bool, m *project.Manifest) error {
	targets, err := preflightPersonaRemoval(name, m)
	if err != nil {
		return err
	}
	staged := make([]stagedRemoval, 0, len(targets))
	rollback := func(primary error) error {
		for i := len(staged) - 1; i >= 0; i-- {
			item := staged[i]
			if err := personaRemoveOps.restore(item.staged, item.target); err != nil {
				primary = fmt.Errorf("%v; restore %s: %w", primary, item.target, err)
			}
			if err := personaRemoveOps.remove(item.dir); err != nil && !errors.Is(err, os.ErrNotExist) {
				primary = fmt.Errorf("%v; cleanup removal stage for %s: %w", primary, item.target, err)
			}
		}
		return primary
	}
	for _, path := range targets {
		stageDir, err := personaRemoveOps.mkdirTemp(filepath.Dir(path), ".shipmates-remove-*")
		if err != nil {
			return rollback(fmt.Errorf("prepare removal of %s: %w", path, err))
		}
		stagedPath := filepath.Join(stageDir, filepath.Base(path))
		if err := personaRemoveOps.stage(path, stagedPath); err != nil {
			_ = personaRemoveOps.remove(stageDir)
			return rollback(fmt.Errorf("remove %s: %w", path, err))
		}
		staged = append(staged, stagedRemoval{target: path, staged: stagedPath, dir: stageDir})
		delete(m.Files, path)
	}
	if err := personaRemoveOps.save(m); err != nil {
		var uncertain *project.ManifestCommitUncertainError
		if !errors.As(err, &uncertain) {
			return rollback(fmt.Errorf("save manifest after removing persona %q: %w", name, err))
		}
		// The rename crossed the commit boundary. Keep filesystem state aligned
		// with the installed manifest; the caller must treat durability as uncertain.
		for _, item := range staged {
			_ = personaRemoveOps.remove(item.staged)
			_ = personaRemoveOps.remove(item.dir)
		}
		return err
	}
	for _, item := range staged {
		if err := personaRemoveOps.remove(item.staged); err != nil {
			return fmt.Errorf("cleanup removed artifact %s: %w", item.target, err)
		}
		if err := personaRemoveOps.remove(item.dir); err != nil {
			return fmt.Errorf("cleanup removal stage for %s: %w", item.target, err)
		}
		slog.Info("removed managed persona artifact", "persona", name, "path", item.target)
	}
	if purge {
		memDir := project.MemoryDir(name)
		if err := os.RemoveAll(memDir); err != nil {
			return err
		}
		slog.Info("purged persona memory", "persona", name, "dir", memDir)
	} else {
		slog.Info("memory preserved", "persona", name, "dir", project.MemoryDir(name))
	}
	return nil
}

// preflightPersonaRemoval validates the complete managed target set before any
// deletion, preventing a modified policy from causing a partial agent removal.
func preflightPersonaRemoval(name string, m *project.Manifest) ([]string, error) {
	managed := []string{project.CodexAgentPath(name), project.PolicyPath(name)}
	var targets []string
	for _, path := range managed {
		baseline, tracked := m.Files[path]
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			if tracked {
				return nil, fmt.Errorf("refusing to remove persona %q: tracked managed target %s is missing", name, path)
			}
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("refusing to remove persona %q: managed target %s is not a regular file", name, path)
		}
		if !tracked {
			return nil, fmt.Errorf("refusing to remove persona %q: managed target %s is untracked", name, path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", path, err)
		}
		if project.SHA(raw) != baseline {
			return nil, fmt.Errorf("refusing to remove persona %q: managed target %s was modified", name, path)
		}
		targets = append(targets, path)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("persona %q is not installed", name)
	}
	if _, tracked := m.Files[project.CodexAgentPath(name)]; !tracked {
		return nil, fmt.Errorf("refusing to remove persona %q: canonical Codex agent is untracked", name)
	}
	return targets, nil
}
