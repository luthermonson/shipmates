// Package commands — hook subcommand.
//
// The `shipmates hook` group is invoked by Claude Code hooks configured in
// `.claude/settings.json`. Right now there's one hook: load-memory, which is
// wired to Claude Code's SessionStart event and injects the active persona's
// `.shipmates/memory/<persona>/` contents as SessionStart context on every
// launch — first-run AND resume.
//
// The motivating problem is documented in docs/memory-on-session-start.md.
// Short version: shipmates promises memory is "auto-loaded on session start,"
// but the promise was previously kept by a prompt instruction inside each
// persona file ("Read your memory first"), which the model can and does skip.
// Making the harness do the load — via a SessionStart command hook — turns
// the promise from discretionary into deterministic.
package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

// Hook is the `shipmates hook` subcommand group. Subcommands are invoked by
// Claude Code hook config in `.claude/settings.json`; users don't run them
// directly.
func Hook() *cli.Command {
	return &cli.Command{
		Name:  "hook",
		Usage: "internal — commands invoked by Claude Code hooks in .claude/settings.json",
		Commands: []*cli.Command{
			{
				Name:  "load-memory",
				Usage: "SessionStart hook: read stdin (CC hook payload), print .shipmates/memory/<agent_type>/ contents as additionalContext",
				Action: func(ctx context.Context, c *cli.Command) error {
					return runLoadMemory(os.Stdin, os.Stdout, os.Stderr)
				},
			},
			{
				Name:  "brig-reminder",
				Usage: "SessionStart hook: remind the session it is bound by the Ship's Articles (and announce an engaged freeze)",
				Action: func(ctx context.Context, c *cli.Command) error {
					return runBrigReminder(os.Stdout, os.Stderr)
				},
			},
		},
	}
}

// runBrigReminder is the brig-reminder hook body. It is installed
// unconditionally alongside load-memory and gates itself here on the
// operator's brig config: a disabled brig emits nothing, so toggling
// `brig.enabled` in ~/.shipmates/config.yaml takes effect on the next
// session with no re-install. Fails soft like load-memory — a broken hook
// must never prevent a session from starting.
func runBrigReminder(stdout, stderr io.Writer) error {
	settings := brig.Load("")
	block := brig.PromptBlock(settings)
	if block == "" {
		return nil
	}
	ctx := block
	if !settings.Disabled("respect-the-freeze") {
		if frozen, marker := brig.CheckFreeze("."); frozen {
			ctx += "\n\nTHE FREEZE IS ENGAGED"
			if marker != nil && marker.Reason != "" {
				ctx += " (reason: " + marker.Reason
				if marker.Admiral != "" {
					ctx += ", admiral: " + marker.Admiral
				}
				ctx += ")"
			}
			ctx += ". Refuse all Write and Edit operations until `shipmates release` clears it."
		}
	}
	out := hookOutput{
		HookSpecificOutput: hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: ctx,
		},
	}
	if err := json.NewEncoder(stdout).Encode(out); err != nil {
		fmt.Fprintf(stderr, "shipmates hook brig-reminder: write output: %v\n", err)
	}
	return nil
}

// sessionStartPayload is the subset of Claude Code's SessionStart hook stdin
// payload we care about. The full payload also carries session_id, cwd,
// source (startup|resume), and the transcript path — none of which affect
// what we load. Extra fields decode fine because encoding/json ignores
// unknown keys by default.
type sessionStartPayload struct {
	AgentType string `json:"agent_type"`
}

// hookOutput is the response shape Claude Code injects into the session for
// SessionStart hooks. `additionalContext` is prepended to the model's system
// context. This mirrors the shape server.go already uses for PreToolUse
// (`hookSpecificOutput.hookEventName + hookEventName-specific fields`).
type hookOutput struct {
	HookSpecificOutput hookSpecificOutput `json:"hookSpecificOutput"`
}

type hookSpecificOutput struct {
	HookEventName     string `json:"hookEventName"`
	AdditionalContext string `json:"additionalContext,omitempty"`
}

// runLoadMemory is the load-memory hook body: parse the CC payload from stdin,
// walk the persona's memory dir, format the contents, and emit CC's
// SessionStart hook response on stdout.
//
// Fails soft:
//   - Missing agent_type → print nothing, exit 0 (unknown context; nothing to load).
//   - Missing memory dir → print nothing, exit 0 (fresh persona; nothing yet).
//   - Bad JSON on stdin → warn to stderr, exit 0 (don't wedge the session).
//   - Read errors on individual files → warn to stderr, skip the file, continue.
//
// The bias is aggressive: a broken hook must NEVER prevent Claude Code from
// launching. If we can't load memory we still let the session start; the model
// falls back to its previous behavior (persona file's "read memory" prompt),
// which is where we've been all along.
func runLoadMemory(stdin io.Reader, stdout, stderr io.Writer) error {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintf(stderr, "shipmates hook load-memory: read stdin: %v\n", err)
		return nil
	}
	var payload sessionStartPayload
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			fmt.Fprintf(stderr, "shipmates hook load-memory: parse payload: %v\n", err)
			return nil
		}
	}
	persona := strings.TrimSpace(payload.AgentType)
	if persona == "" {
		// No persona in the payload — nothing to load. This happens when
		// Claude Code launches without --agent (e.g. someone runs bare
		// `claude` in the repo). Silently no-op.
		return nil
	}

	ctx, err := buildMemoryContext(persona)
	if err != nil {
		fmt.Fprintf(stderr, "shipmates hook load-memory: %v\n", err)
		return nil
	}
	if ctx == "" {
		return nil
	}

	out := hookOutput{
		HookSpecificOutput: hookSpecificOutput{
			HookEventName:     "SessionStart",
			AdditionalContext: ctx,
		},
	}
	enc := json.NewEncoder(stdout)
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(stderr, "shipmates hook load-memory: write output: %v\n", err)
	}
	return nil
}

// buildMemoryContext walks .shipmates/memory/<persona>/ and returns a single
// string containing every .md file's contents, each prefixed with a header
// naming the source file. Returns "" if the dir doesn't exist or is empty.
//
// Sort order is filename-lexicographic so the output is stable across runs —
// useful for debugging and for keeping prompt-caching hit rates high in
// downstream tooling that fingerprints system-context blocks.
func buildMemoryContext(persona string) (string, error) {
	dir := project.MemoryDir(persona)
	info, err := os.Stat(dir)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", dir, err)
	}
	if !info.IsDir() {
		return "", nil
	}

	// Collect .md files. Non-md files are skipped: memory is markdown by
	// convention (catalog seeds are all .md), and letting stray files
	// (editor swap files, .DS_Store, .bak) into the context would be a
	// footgun with no upside.
	var files []string
	err = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			// Directory-level read error — log and skip this branch.
			slog.Warn("hook load-memory: walk error", "path", path, "err", err)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".md") {
			return nil
		}
		files = append(files, path)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", dir, err)
	}
	if len(files) == 0 {
		return "", nil
	}
	sort.Strings(files)

	var b strings.Builder
	fmt.Fprintf(&b, "# %s memory (auto-loaded by shipmates on session start)\n\n", persona)
	fmt.Fprintf(&b, "Loaded from `%s/`. %d file(s). This is the persistent, per-project memory "+
		"you wrote to yourself in previous sessions — treat it as your working context, "+
		"not fresh reading material.\n\n", filepath.ToSlash(dir), len(files))
	for _, path := range files {
		content, err := os.ReadFile(path)
		if err != nil {
			// Individual-file read error — log and keep going. A single
			// unreadable file must not block the rest.
			slog.Warn("hook load-memory: read failed", "path", path, "err", err)
			continue
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		fmt.Fprintf(&b, "\n## %s\n\n", filepath.ToSlash(rel))
		b.Write(content)
		if len(content) > 0 && content[len(content)-1] != '\n' {
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}
