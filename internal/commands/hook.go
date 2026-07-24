package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// hookMemoryBudget bounds how much memory content `hook load-memory` prints
// in total. SessionStart hook stdout is injected into the session context,
// so an unbounded dump could crowd out the actual conversation.
const hookMemoryBudget = 8 * 1024

// Hook groups the internal endpoints that runtime-native hook mechanisms
// invoke. The Claude Code runtime installs a SessionStart hook in
// .claude/settings.json that runs `shipmates hook load-memory` (see
// internal/runtime/claude.InstallMemoryHook); this command is its
// implementation. Hidden because operators never run it by hand.
func Hook() *cli.Command {
	return &cli.Command{
		Name:   "hook",
		Usage:  "internal endpoints invoked by runtime hook mechanisms",
		Hidden: true,
		Commands: []*cli.Command{
			{
				Name:  "load-memory",
				Usage: "print persona memory to stdout for SessionStart context injection",
				Action: func(_ context.Context, c *cli.Command) error {
					// A hook failure must never break a session: swallow every
					// error, print whatever memory we could resolve, exit 0.
					printHookMemory(c.Writer)
					return nil
				},
			},
		},
	}
}

// printHookMemory writes persona memory to w, best-effort.
//
// Persona resolution: the Claude Code SessionStart hook receives session
// JSON on stdin but not the active agent/persona name, so the claude
// runtime exports SHIPMATES_PERSONA into the spawned process environment
// (see internal/runtime/claude.Runtime.SendTurn) and the hook — which
// inherits that environment — reads it back here. When the variable is
// absent (e.g. an operator launched `claude` by hand inside the project),
// we print every installed persona's memory instead, still bounded by
// hookMemoryBudget.
func printHookMemory(w io.Writer) {
	root, err := project.FindRoot(".")
	if err != nil {
		return // not inside a shipmates project — print nothing
	}
	memRoot := filepath.Join(root, project.Dir, project.MemoryDirName)

	budget := hookMemoryBudget
	if persona := strings.TrimSpace(os.Getenv("SHIPMATES_PERSONA")); persona != "" {
		if project.ValidatePersonaName(persona) == nil {
			printPersonaMemory(w, memRoot, persona, &budget)
		}
		return
	}

	// No persona in the environment: print all personas' memory, bounded.
	entries, err := os.ReadDir(memRoot)
	if err != nil {
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() && project.ValidatePersonaName(e.Name()) == nil {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if budget <= 0 {
			return
		}
		printPersonaMemory(w, memRoot, name, &budget)
	}
}

// printPersonaMemory prints every regular file under memRoot/persona with a
// header per file, consuming from *budget. Files are visited in sorted
// order so output is deterministic.
func printPersonaMemory(w io.Writer, memRoot, persona string, budget *int) {
	dir := filepath.Join(memRoot, persona)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	files := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Type().IsRegular() {
			files = append(files, e.Name())
		}
	}
	sort.Strings(files)
	for _, name := range files {
		if *budget <= 0 {
			return
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || len(raw) == 0 {
			continue
		}
		if len(raw) > *budget {
			raw = raw[:*budget]
		}
		fmt.Fprintf(w, "## shipmates memory: %s/%s\n\n", persona, name)
		w.Write(raw)
		if raw[len(raw)-1] != '\n' {
			io.WriteString(w, "\n")
		}
		io.WriteString(w, "\n")
		*budget -= len(raw)
	}
}
