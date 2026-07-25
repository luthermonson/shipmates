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

// runUpdate refreshes canonical Codex personas and policies from the embedded
// catalog, applying the four-case logic (see reconcileFile). Memory, sessions,
// and unmanaged artifacts are never touched.
func runUpdate(cat *catalog.Catalog, only, accept string) error {
	migrated, err := migrateLegacyCaptain(cat)
	if err != nil {
		return err
	}
	if only == "captain" && migrated {
		return nil
	}
	m, err := project.LoadManifest()
	if err != nil {
		return err
	}
	if err := requireManifestV2(m, "update"); err != nil {
		return err
	}
	return withPolicyWriteLock(func() error { return runUpdateLocked(cat, only, accept, m) })
}

// migrateLegacyCaptain installs the Phase 2 leadership split and removes only
// unchanged Shipmates-owned captain artifacts. Memory and edited files are
// preserved; the quartermaster receives a durable pointer to legacy memory.
func migrateLegacyCaptain(cat *catalog.Catalog) (bool, error) {
	if !cat.Has("quartermaster") || !cat.Has("skipper") {
		return false, nil
	}
	if _, err := os.Lstat(project.CodexAgentPath("captain")); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	m, err := project.LoadManifest()
	if err != nil {
		return false, err
	}
	if err := requireManifestV2(m, "update"); err != nil {
		return false, err
	}
	err = withPolicyWriteLock(func() error {
		for _, persona := range []string{"quartermaster", "skipper"} {
			if err := addPersonaLocked(cat, persona, m); err != nil {
				return fmt.Errorf("install Phase 2 %s: %w", persona, err)
			}
		}
		return archiveLegacyCaptain(m)
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

func archiveLegacyCaptain(m *project.Manifest) error {
	modified := make([]string, 0)
	for _, path := range []string{project.CodexAgentPath("captain"), project.PolicyPath("captain")} {
		baseline, managed := m.Files[path]
		if !managed {
			if _, err := os.Lstat(path); err == nil {
				modified = append(modified, path)
				if err := archiveCaptainArtifact(path); err != nil {
					return err
				}
			}
			continue
		}
		body, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			delete(m.Files, path)
			continue
		}
		if err != nil {
			return err
		}
		if project.SHA(body) != baseline {
			modified = append(modified, path)
			if err := archiveCaptainArtifact(path); err != nil {
				return err
			}
			delete(m.Files, path)
			continue
		}
		if err := os.Remove(path); err != nil {
			return err
		}
		delete(m.Files, path)
		slog.Info("removed unchanged legacy captain artifact", "path", path)
	}
	if err := m.Save(); err != nil {
		return err
	}
	legacyMemory := filepath.Join(project.Dir, project.MemoryDirName, "captain")
	if info, err := os.Stat(legacyMemory); err == nil && info.IsDir() {
		quartermasterMemory := filepath.Join(project.Dir, project.MemoryDirName, "quartermaster")
		if err := os.MkdirAll(quartermasterMemory, 0o755); err != nil {
			return err
		}
		note := filepath.Join(quartermasterMemory, "legacy-captain-memory.md")
		if _, err := os.Stat(note); errors.Is(err, os.ErrNotExist) {
			content := "# Legacy captain memory\n\nPhase 2 reserves captain for the human. Review preserved strategic memory in `../captain/`; migrate durable knowledge deliberately without deleting its source.\n"
			if err := os.WriteFile(note, []byte(content), 0o600); err != nil {
				return err
			}
		}
	}
	if len(modified) > 0 {
		slog.Warn("legacy captain artifacts were archived because they are edited or unmanaged", "paths", strings.Join(modified, ","))
	}
	return nil
}

func archiveCaptainArtifact(path string) error {
	archiveDir := filepath.Join(project.Dir, "legacy", "captain")
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		return err
	}
	name := "agent.toml"
	if strings.HasSuffix(path, ".yaml") {
		name = "policy.yaml"
	}
	destination := filepath.Join(archiveDir, name)
	if _, err := os.Lstat(destination); err == nil {
		return fmt.Errorf("legacy captain archive already exists: %s", destination)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(path, destination)
}

func runUpdateLocked(cat *catalog.Catalog, only, accept string, m *project.Manifest) error {

	st := &updateState{
		in:          bufio.NewScanner(os.Stdin),
		interactive: accept == "" && isInteractive(),
	}
	switch accept {
	case "ours":
		st.stickyAll, st.stickyRes = true, resKeep
	case "theirs":
		st.stickyAll, st.stickyRes = true, resTake
	}

	names, err := personasToUpdate(cat, m, only)
	if err != nil {
		return err
	}
	for _, name := range names {
		base, err := cat.AgentFile(name)
		if err != nil {
			return fmt.Errorf("read catalog agent %s: %w", name, err)
		}
		catBytes, err := composeAgent(cat, base)
		if err != nil {
			return err
		}
		fm, body := splitPersona(catBytes)
		codexBytes := []byte(renderCodex(fm, body))
		codexPath := project.CodexAgentPath(name)
		if err := reconcileFile(m, st, codexPath, codexBytes, "Codex agent"); err != nil {
			return err
		}
		// The configured runtime's own artifact, from the same composed
		// bytes. Missing-and-untracked installs here rather than being
		// skipped, so `update` is also how a project that switched to claude
		// picks up its `.claude/agents/<persona>.md`.
		if err := reconcileRuntimePersona(m, st, name, catBytes); err != nil {
			return err
		}
		policy, err := catalogM5Policy(cat, name)
		if err != nil {
			return fmt.Errorf("read catalog policy %s: %w", name, err)
		}
		if err := reconcileFile(m, st, project.PolicyPath(name), policy, "policy"); err != nil {
			return err
		}
	}

	if err := m.Save(); err != nil {
		return err
	}
	// Re-assert the configured runtime's memory hook. `update` is how an
	// operator picks up shipmates changes — including a runtime switch in
	// the project config, or a settings.json an editor rewrote.
	installMemoryHook()
	slog.Info("update complete",
		"updated", st.updated, "added", st.added, "kept", st.kept,
		"conflicts", st.conflicts, "alreadyCurrent", st.skipped)
	return nil
}

// updateState carries the running counters and sticky conflict resolution
// across the files reconciled in one `update` run.
type updateState struct {
	in                                       *bufio.Scanner
	interactive                              bool
	stickyAll                                bool
	stickyRes                                resolution
	updated, kept, added, conflicts, skipped int
}

// reconcileFile applies the four-case update logic to one shipmates-managed
// file (Codex persona or policy): re-add if missing-but-recorded, overwrite if
// unchanged-by-user, leave user edits when the catalog is unchanged, and
// diff-prompt when both diverged.
func reconcileFile(m *project.Manifest, st *updateState, dst string, catBytes []byte, label string) error {
	catSHA := project.SHA(catBytes)
	baseline, recorded := m.Files[dst]

	info, statErr := os.Lstat(dst)
	missing := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !missing {
		return fmt.Errorf("inspect %s: %w", dst, statErr)
	}
	if !missing && !info.Mode().IsRegular() {
		return fmt.Errorf("refusing to update managed target %s: not a regular file", dst)
	}
	var onDisk []byte
	if !missing {
		onDisk, statErr = os.ReadFile(dst)
		if statErr != nil {
			return fmt.Errorf("read %s: %w", dst, statErr)
		}
	}

	if missing {
		if !recorded {
			return nil // never installed
		}
		if err := writeManaged(dst, catBytes); err != nil {
			return err
		}
		m.Files[dst] = catSHA
		slog.Info("re-added missing "+label, "path", dst)
		st.added++
		return nil
	}

	diskSHA := project.SHA(onDisk)

	// Already identical to the catalog — nothing to do (track it if untracked).
	if diskSHA == catSHA {
		if !recorded {
			m.Files[dst] = catSHA
		}
		st.skipped++
		return nil
	}

	// Shipmates-installed and unchanged by the user → safe to overwrite.
	if recorded && diskSHA == baseline {
		if err := writeManaged(dst, catBytes); err != nil {
			return err
		}
		m.Files[dst] = catSHA
		slog.Info("updated "+label, "path", dst)
		st.updated++
		return nil
	}

	// User-edited but the catalog hasn't moved → leave their edits alone.
	if recorded && catSHA == baseline {
		slog.Debug("user-edited, catalog unchanged; leaving alone", "path", dst)
		st.kept++
		return nil
	}

	// CONFLICT — never clobber silently. Either the user edited a shipmates file
	// AND the catalog moved, OR this is a pre-existing file shipmates didn't
	// install (unrecorded). Both require the user's decision.
	st.conflicts++
	res := st.stickyRes
	if !st.stickyAll {
		if !st.interactive {
			slog.Warn("conflict (non-interactive); keeping your version",
				"path", dst, "yours", short(diskSHA), "baseline", short(baseline), "shipped", short(catSHA))
			st.kept++
			return nil
		}
		r, all, err := promptConflict(st.in, dst, onDisk, catBytes, diskSHA, baseline, catSHA)
		if err != nil {
			return err
		}
		res = r
		if all {
			st.stickyAll, st.stickyRes = true, r
		}
	}

	switch res {
	case resTake:
		if err := writeManaged(dst, catBytes); err != nil {
			return err
		}
		m.Files[dst] = catSHA
		slog.Info("took shipped version", "path", dst)
		st.updated++
	case resSidecar:
		side := dst + ".new"
		if err := writeManaged(side, catBytes); err != nil {
			return err
		}
		slog.Info("wrote sidecar; merge manually", "path", side)
		st.kept++
	default:
		slog.Info("kept your version", "path", dst)
		st.kept++
	}
	return nil
}

// personasToUpdate is the set of installed personas to consider. A non-empty
// `only` narrows to a single persona.
func personasToUpdate(cat *catalog.Catalog, m *project.Manifest, only string) ([]string, error) {
	if only != "" {
		if err := project.ValidatePersonaName(only); err != nil {
			return nil, err
		}
		if !cat.Has(only) {
			return nil, fmt.Errorf("unknown persona %q", only)
		}
		path := project.CodexAgentPath(only)
		_, recorded := m.Files[path]
		if _, err := os.Lstat(path); err == nil || recorded {
			return []string{only}, nil
		}
		return nil, fmt.Errorf("persona %q is not installed", only)
	}
	avail, err := cat.Personas()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, name := range avail {
		dst := project.CodexAgentPath(name)
		if _, recorded := m.Files[dst]; recorded {
			out = append(out, name)
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			out = append(out, name)
		}
	}
	return out, nil
}

// writeManaged writes a shipmates-managed file, creating parent dirs as needed.
func writeManaged(dst string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		return err
	}
	// WriteFile preserves the mode of an existing file. Chmod only after the
	// caller has validated/read that managed regular file, so upgrades safely
	// migrate historical 0644 installs without widening an untrusted target.
	return os.Chmod(dst, 0o600)
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
