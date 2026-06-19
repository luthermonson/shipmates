package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Update refreshes installed files from the embedded catalog with diff-on-conflict.
func Update(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "update",
		Usage:     "refresh installed files from the embedded catalog (diff on conflict)",
		ArgsUsage: "[persona]",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "accept", Usage: "non-interactive conflict resolution: ours|theirs"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			accept := strings.ToLower(strings.TrimSpace(c.String("accept")))
			if accept != "" && accept != "ours" && accept != "theirs" {
				return fmt.Errorf("--accept must be ours|theirs, got %q", accept)
			}
			return runUpdate(cat, c.Args().First(), accept)
		},
	}
}

// resolution is a per-conflict (or sticky "for all") decision.
type resolution int

const (
	resKeep    resolution = iota // keep the user's version
	resTake                      // take the catalog's version
	resSidecar                   // write catalog version to <file>.new
)

// runUpdate applies the four-case logic from docs/architecture.md to each
// installed persona's agent file. Memory is never touched.
func runUpdate(cat *catalog.Catalog, only, accept string) error {
	m, err := project.LoadManifest()
	if err != nil {
		return err
	}

	interactive := accept == "" && isInteractive()

	var stickyAll bool
	var stickyRes resolution
	switch accept {
	case "ours":
		stickyAll, stickyRes = true, resKeep
	case "theirs":
		stickyAll, stickyRes = true, resTake
	}

	names, err := personasToUpdate(cat, m, only)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		slog.Info("nothing installed to update")
		return nil
	}

	in := bufio.NewScanner(os.Stdin)
	var updated, kept, added, conflicts, skipped int

	for _, name := range names {
		dst := project.AgentPath(name)
		baseline, recorded := m.Files[dst]
		catBytes, err := cat.AgentFile(name)
		if err != nil {
			return fmt.Errorf("read catalog agent %s: %w", name, err)
		}
		catSHA := project.SHA(catBytes)

		onDisk, statErr := os.ReadFile(dst)
		missing := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !missing {
			return fmt.Errorf("read %s: %w", dst, statErr)
		}

		if missing {
			if !recorded {
				continue // orphan / never installed
			}
			if err := writeAgent(dst, catBytes); err != nil {
				return err
			}
			m.Files[dst] = catSHA
			slog.Info("re-added missing persona", "persona", name, "path", dst)
			added++
			continue
		}

		diskSHA := project.SHA(onDisk)

		if !recorded || diskSHA == baseline {
			if diskSHA == catSHA {
				skipped++ // already current
				continue
			}
			if err := writeAgent(dst, catBytes); err != nil {
				return err
			}
			m.Files[dst] = catSHA
			slog.Info("updated persona", "persona", name, "path", dst)
			updated++
			continue
		}

		// User-edited (diskSHA != baseline).
		if catSHA == baseline {
			slog.Debug("user-edited, catalog unchanged; leaving alone", "path", dst)
			kept++
			continue
		}

		// CONFLICT: both diverged.
		conflicts++
		res := stickyRes
		if !stickyAll {
			if !interactive {
				slog.Warn("conflict (non-interactive); keeping your version",
					"path", dst, "yours", short(diskSHA), "baseline", short(baseline), "shipped", short(catSHA))
				kept++
				continue
			}
			r, all, err := promptConflict(in, dst, onDisk, catBytes, diskSHA, baseline, catSHA)
			if err != nil {
				return err
			}
			res = r
			if all {
				stickyAll, stickyRes = true, r
			}
		}

		switch res {
		case resTake:
			if err := writeAgent(dst, catBytes); err != nil {
				return err
			}
			m.Files[dst] = catSHA
			slog.Info("took shipped version", "path", dst)
			updated++
		case resSidecar:
			side := dst + ".new"
			if err := writeAgent(side, catBytes); err != nil {
				return err
			}
			slog.Info("wrote sidecar; merge manually", "path", side)
			kept++
		default: // resKeep
			slog.Info("kept your version", "path", dst)
			kept++
		}
	}

	if err := m.Save(); err != nil {
		return err
	}
	slog.Info("update complete",
		"updated", updated, "added", added, "kept", kept,
		"conflicts", conflicts, "alreadyCurrent", skipped)
	return nil
}

// personasToUpdate is the set of installed personas to consider. A non-empty
// `only` narrows to a single persona.
func personasToUpdate(cat *catalog.Catalog, m *project.Manifest, only string) ([]string, error) {
	if only != "" {
		if !cat.Has(only) {
			return nil, fmt.Errorf("unknown persona %q", only)
		}
		return []string{only}, nil
	}
	avail, err := cat.Personas()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range avail {
		dst := project.AgentPath(name)
		if _, recorded := m.Files[dst]; recorded {
			out = append(out, name)
			continue
		}
		if _, err := os.Stat(dst); err == nil {
			out = append(out, name)
		}
	}
	return out, nil
}

// writeAgent writes an agent file, creating parent dirs as needed.
func writeAgent(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// isInteractive reports whether stdin is a character device (a terminal).
func isInteractive() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// promptConflict renders the diff and the menu, then reads a single choice.
func promptConflict(in *bufio.Scanner, dst string, yours, theirs []byte, yourSHA, baseSHA, theirSHA string) (resolution, bool, error) {
	printConflictHeader(dst, yourSHA, baseSHA, theirSHA)
	fmt.Print(unifiedDiff(string(yours), string(theirs)))
	for {
		fmt.Print("\n" + conflictMenu + "\n  > ")
		if !in.Scan() {
			fmt.Println("(eof) keeping your version")
			return resKeep, false, in.Err()
		}
		switch strings.TrimSpace(in.Text()) {
		case "", "k":
			return resKeep, false, nil
		case "t":
			return resTake, false, nil
		case "s":
			return resSidecar, false, nil
		case "d":
			fmt.Print(unifiedDiff(string(yours), string(theirs)))
		case "a":
			return resKeep, true, nil
		case "T":
			return resTake, true, nil
		default:
			fmt.Println("  unrecognized choice; try again")
		}
	}
}

const conflictMenu = `  [k] keep your version              (default)
  [t] take the new shipped version
  [s] save shipped as <file>.new     (sidecar; merge manually)
  [d] re-show diff
  [a] keep yours for all remaining conflicts
  [T] take theirs for all remaining conflicts`

func printConflictHeader(dst, yourSHA, baseSHA, theirSHA string) {
	fmt.Printf("\nConflict: %s\n", dst)
	fmt.Printf("  Your version (sha: %s) diverges from baseline (sha: %s).\n", short(yourSHA), short(baseSHA))
	fmt.Printf("  Catalog has a new version (sha: %s).\n\n", short(theirSHA))
}

// unifiedDiff produces a minimal line-based diff of a -> b using an LCS over
// lines. Not git-perfect; it shows the user what changed.
func unifiedDiff(a, b string) string {
	al, bl := splitLines(a), splitLines(b)
	n, m := len(al), len(bl)
	c := make([][]int, n+1)
	for i := range c {
		c[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if al[i] == bl[j] {
				c[i][j] = c[i+1][j+1] + 1
			} else if c[i+1][j] >= c[i][j+1] {
				c[i][j] = c[i+1][j]
			} else {
				c[i][j] = c[i][j+1]
			}
		}
	}
	var sb strings.Builder
	sb.WriteString("  --- your version\n  +++ shipped version\n")
	i, j := 0, 0
	for i < n && j < m {
		switch {
		case al[i] == bl[j]:
			sb.WriteString("   " + al[i] + "\n")
			i, j = i+1, j+1
		case c[i+1][j] >= c[i][j+1]:
			sb.WriteString("  -" + al[i] + "\n")
			i++
		default:
			sb.WriteString("  +" + bl[j] + "\n")
			j++
		}
	}
	for ; i < n; i++ {
		sb.WriteString("  -" + al[i] + "\n")
	}
	for ; j < m; j++ {
		sb.WriteString("  +" + bl[j] + "\n")
	}
	return sb.String()
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
