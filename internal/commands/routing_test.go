package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

// routingCatalog is a minimal catalog carrying a github routing template that
// exercises every conditional the real template uses.
func routingCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
		"catalog/routing/github.md": {Data: []byte(
			"## GitHub routing\n\n" +
				"{{if .Bylines}}BYLINES ON{{end}}\n\n\n" +
				"{{if .Labels}}LABELS ON{{end}}\n\n" +
				"{{if .Beads}}BEADS ON{{end}}\n")},
		"catalog/commands/standup.md":      {Data: []byte("standup command v1\n")},
		"catalog/commands/sync-routing.md": {Data: []byte("sync-routing v1\n")},
	})
}

// ---------------------------------------------------------------------------
// stripRoutingBlock / applyRouting
// ---------------------------------------------------------------------------

func TestStripRoutingBlock(t *testing.T) {
	t.Run("removes the block and leaves the persona body", func(t *testing.T) {
		in := "---\nname: geordi\n---\n\n# Geordi\n\n" +
			"<!-- shipmates:routing:github -->\nROUTING\n<!-- /shipmates:routing:github -->\n"
		got := string(stripRoutingBlock([]byte(in)))
		if strings.Contains(got, "shipmates:routing") || strings.Contains(got, "ROUTING") {
			t.Errorf("routing block survived:\n%s", got)
		}
		if !strings.Contains(got, "# Geordi") {
			t.Errorf("persona body lost:\n%s", got)
		}
		if !strings.HasSuffix(got, "\n") {
			t.Errorf("result should end with a single newline: %q", got)
		}
	})

	t.Run("preserves content that follows the block", func(t *testing.T) {
		in := "# Geordi\n\n<!-- shipmates:routing:github -->\nROUTING\n<!-- /shipmates:routing:github -->\n\n## Trailing section\nkeep me\n"
		got := string(stripRoutingBlock([]byte(in)))
		if !strings.Contains(got, "## Trailing section") || !strings.Contains(got, "keep me") {
			t.Errorf("trailing content dropped:\n%s", got)
		}
		if strings.Contains(got, "ROUTING") {
			t.Errorf("routing block survived:\n%s", got)
		}
	})

	t.Run("no block is a passthrough", func(t *testing.T) {
		in := []byte("# Geordi\n\nno routing here\n")
		if got := string(stripRoutingBlock(in)); got != string(in) {
			t.Errorf("content changed with no block present:\n%q", got)
		}
	})

	t.Run("a lone opening marker is left alone", func(t *testing.T) {
		// Half a block means something odd happened; better to leave the file
		// untouched than to guess where the block ends.
		in := []byte("# Geordi\n\n<!-- shipmates:routing:github -->\nROUTING\n")
		if got := string(stripRoutingBlock(in)); got != string(in) {
			t.Errorf("unterminated block was edited:\n%q", got)
		}
	})
}

// applyRouting must be idempotent: re-running it after the template changes
// replaces the old block rather than stacking a second one.
func TestApplyRouting_IsIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cat := routingCatalog()
	base := []byte("---\nname: custom\n---\n\n# Custom persona\n")

	first, err := applyRouting(cat, base)
	if err != nil {
		t.Fatal(err)
	}
	second, err := applyRouting(cat, first)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Errorf("applyRouting is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	if n := strings.Count(string(second), "<!-- shipmates:routing:github -->"); n != 1 {
		t.Errorf("routing markers stacked: %d opening markers\n%s", n, second)
	}
	if !strings.Contains(string(second), "# Custom persona") {
		t.Errorf("persona body lost across re-apply:\n%s", second)
	}
}

// A custom persona that already carries an OLD routing block gets the current
// one — the whole point of `routing apply` after a template bump.
func TestApplyRouting_ReplacesAStaleBlock(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := []byte("# Custom\n\n<!-- shipmates:routing:github -->\nSTALE RULES\n<!-- /shipmates:routing:github -->\n")
	out, err := applyRouting(routingCatalog(), stale)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "STALE RULES") {
		t.Errorf("stale routing rules survived:\n%s", out)
	}
	if !strings.Contains(string(out), "GitHub routing") {
		t.Errorf("current routing block not applied:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// renderRoutingBlock — routingOptions and the beads seam
// ---------------------------------------------------------------------------

func TestRenderRoutingBlock(t *testing.T) {
	cat := routingCatalog()

	t.Run("no routing declared yields no block", func(t *testing.T) {
		t.Chdir(t.TempDir())
		b, err := renderRoutingBlock(cat)
		if err != nil {
			t.Fatal(err)
		}
		if b != nil {
			t.Errorf("expected nil block, got %q", b)
		}
	})

	t.Run("unknown routing name yields no block, not an error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: gitlab\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := renderRoutingBlock(cat)
		if err != nil {
			t.Fatalf("unknown routing should not be an error: %v", err)
		}
		if b != nil {
			t.Errorf("expected nil block, got %q", b)
		}
	})

	t.Run("bylines and labels default to on", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := renderRoutingBlock(cat)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "BYLINES ON") || !strings.Contains(string(b), "LABELS ON") {
			t.Errorf("private-fleet defaults not applied:\n%s", b)
		}
	})

	t.Run("routingOptions can turn bylines and labels off", func(t *testing.T) {
		t.Chdir(t.TempDir())
		cfg := "routing: github\nroutingOptions:\n  bylines: false\n  labels: false\n"
		if err := os.WriteFile(project.ConfigName, []byte(cfg), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := renderRoutingBlock(cat)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "BYLINES ON") || strings.Contains(string(b), "LABELS ON") {
			t.Errorf("open-source options not honored:\n%s", b)
		}
	})

	t.Run("the beads section appears only with a .beads dir", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := renderRoutingBlock(cat)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "BEADS ON") {
			t.Errorf("beads section rendered without a .beads dir:\n%s", b)
		}
		if err := os.Mkdir(".beads", 0o755); err != nil {
			t.Fatal(err)
		}
		b, err = renderRoutingBlock(cat)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "BEADS ON") {
			t.Errorf("beads section missing in a beads workspace:\n%s", b)
		}
	})

	t.Run("template conditionals do not leave blank-line craters", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		b, err := renderRoutingBlock(cat)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "\n\n\n") {
			t.Errorf("blank lines not collapsed:\n%q", b)
		}
	})
}

func TestBeadsWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	if beadsWorkspace() {
		t.Error("empty dir reported as a beads workspace")
	}
	// A *file* named .beads is not a beads workspace.
	if err := os.WriteFile(".beads", []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if beadsWorkspace() {
		t.Error("a regular file named .beads was treated as a workspace")
	}
	if err := os.Remove(".beads"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(".beads", 0o755); err != nil {
		t.Fatal(err)
	}
	if !beadsWorkspace() {
		t.Error(".beads dir not detected")
	}
}

// composeAgent's routingOnBoot mode swaps the whole block for a one-liner so
// persona files stay tiny.
func TestComposeAgent_RoutingOnBoot(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(project.ConfigName, []byte("routing: github\nroutingOnBoot: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := composeAgent(routingCatalog(), []byte("---\nname: geordi\n---\n\n# Geordi\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "/sync-routing") {
		t.Errorf("boot-mode instruction missing:\n%s", s)
	}
	if strings.Contains(s, "GitHub routing") {
		t.Errorf("full routing block embedded despite routingOnBoot: true:\n%s", s)
	}
	if !strings.Contains(s, "<!-- shipmates:routing:github -->") {
		t.Errorf("markers missing in boot mode (update could not strip it):\n%s", s)
	}
}

func TestCollapseBlankLines(t *testing.T) {
	got := string(collapseBlankLines([]byte("\n\na\n\n\n\n\nb\n\n\n")))
	if got != "a\n\nb\n" {
		t.Errorf("collapseBlankLines = %q, want %q", got, "a\n\nb\n")
	}
}

// ---------------------------------------------------------------------------
// routing apply / routing show — argument and config handling
// ---------------------------------------------------------------------------

func TestRoutingCommand(t *testing.T) {
	cat := routingCatalog()

	t.Run("apply refuses when no routing is declared", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := Routing(cat).Run(context.Background(), []string{"routing", "apply", "--all"})
		if err == nil || !strings.Contains(err.Error(), "no routing declared") {
			t.Fatalf("err = %v, want a 'no routing declared' error", err)
		}
	})

	t.Run("apply with neither files nor --all is a usage error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := Routing(cat).Run(context.Background(), []string{"routing", "apply"})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})

	t.Run("apply --all composes fleet personas and skips opt-outs", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		fleetFile := project.AgentPath("geordi")
		optOutFile := project.AgentPath("helper")
		mustWrite(t, fleetFile, "---\nname: geordi\n---\n\n# Geordi\n")
		mustWrite(t, optOutFile, "---\nname: helper\nshipmatesPersona: false\n---\n\n# Helper\n")

		if err := Routing(cat).Run(context.Background(), []string{"routing", "apply", "--all"}); err != nil {
			t.Fatalf("routing apply --all: %v", err)
		}
		if !strings.Contains(readFile(t, fleetFile), "GitHub routing") {
			t.Error("fleet persona did not get the routing block")
		}
		if strings.Contains(readFile(t, optOutFile), "GitHub routing") {
			t.Error("shipmatesPersona: false agent was composed anyway")
		}
	})

	t.Run("show refuses when no routing is declared", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := Routing(cat).Run(context.Background(), []string{"routing", "show"})
		if err == nil || !strings.Contains(err.Error(), "no routing declared") {
			t.Fatalf("err = %v, want a 'no routing declared' error", err)
		}
	})

	t.Run("show prints the active block", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		var err error
		out := captureStdout(t, func() {
			err = Routing(cat).Run(context.Background(), []string{"routing", "show"})
		})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "GitHub routing") {
			t.Errorf("routing show printed nothing useful:\n%s", out)
		}
	})
}

// ---------------------------------------------------------------------------
// installCommands
// ---------------------------------------------------------------------------

func TestInstallCommands(t *testing.T) {
	t.Run("vendors every catalog command and records it", func(t *testing.T) {
		t.Chdir(t.TempDir())
		m := &project.Manifest{Files: map[string]string{}}
		if err := installCommands(routingCatalog(), m); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"standup", "sync-routing"} {
			dst := project.CommandPath(name)
			if _, err := os.Stat(dst); err != nil {
				t.Errorf("command %s not installed: %v", name, err)
			}
			if m.Files[dst] == "" {
				t.Errorf("command %s not recorded in the manifest", name)
			}
		}
	})

	t.Run("does not clobber a user-edited command", func(t *testing.T) {
		t.Chdir(t.TempDir())
		dst := project.CommandPath("standup")
		mine := "MY OWN standup\n"
		mustWrite(t, dst, mine)

		m := &project.Manifest{Files: map[string]string{}}
		if err := installCommands(routingCatalog(), m); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, dst); got != mine {
			t.Errorf("user-edited command clobbered: %q", got)
		}
	})

	t.Run("refreshes a command the user never touched", func(t *testing.T) {
		t.Chdir(t.TempDir())
		dst := project.CommandPath("standup")
		old := "standup command v0\n"
		mustWrite(t, dst, old)
		m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte(old))}}

		if err := installCommands(routingCatalog(), m); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, dst); got != "standup command v1\n" {
			t.Errorf("untouched command not refreshed: %q", got)
		}
	})

	t.Run("a catalog with no commands dir is fine", func(t *testing.T) {
		t.Chdir(t.TempDir())
		empty := catalog.New(fstest.MapFS{"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("x")}})
		m := &project.Manifest{Files: map[string]string{}}
		if err := installCommands(empty, m); err != nil {
			t.Fatalf("empty command set should not error: %v", err)
		}
	})
}

// ---------------------------------------------------------------------------
// defaultConfig — the starter shipmates.yaml must actually parse.
// ---------------------------------------------------------------------------

func TestDefaultConfig_IsValidAndRoundTrips(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(project.ConfigName, []byte(defaultConfig("my-repo")), 0o644); err != nil {
		t.Fatal(err)
	}
	conf, err := project.LoadConfig()
	if err != nil {
		t.Fatalf("the starter config shipmates writes does not parse: %v", err)
	}
	if conf.SessionPrefix != "my-repo" {
		t.Errorf("SessionPrefix = %q, want my-repo", conf.SessionPrefix)
	}
	if conf.Routing != "" {
		t.Errorf("Routing = %q, want empty (routing-agnostic by default)", conf.Routing)
	}
	if conf.SharedMemory {
		t.Error("SharedMemory should default to false (per-developer memory stays gitignored)")
	}
	// The `crew:` example must stay fully commented — an active empty crew key
	// makes a user-appended crew block a duplicate top-level key, which
	// yaml.v3 rejects outright.
	if conf.Crew != nil {
		t.Errorf("crew key is active in the starter config; appending a crew block would break parsing: %v", conf.Crew)
	}
}

func TestDefaultConfig_EmptyPrefix(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(project.ConfigName, []byte(defaultConfig("")), 0o644); err != nil {
		t.Fatal(err)
	}
	conf, err := project.LoadConfig()
	if err != nil {
		t.Fatalf("empty-prefix starter config does not parse: %v", err)
	}
	if conf.SessionPrefix != "" {
		t.Errorf("SessionPrefix = %q, want empty", conf.SessionPrefix)
	}
}

// ---------------------------------------------------------------------------
// gitignoreContainsPattern
// ---------------------------------------------------------------------------

func TestGitignoreContainsPattern(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"exact line", ".shipmates/inbox/\n", true},
		{"with surrounding whitespace", "  .shipmates/inbox/  \n", true},
		{"among other entries", "node_modules/\n.shipmates/inbox/\n*.log\n", true},
		{"commented out does not count", "# .shipmates/inbox/\n", false},
		{"substring of a longer path does not count", ".shipmates/inbox/foo\n", false},
		{"prefix without trailing slash does not count", ".shipmates/inbox\n", false},
		{"empty file", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitignoreContainsPattern([]byte(tc.body), attachInboxIgnorePattern); got != tc.want {
				t.Errorf("got %v, want %v for %q", got, tc.want, tc.body)
			}
		})
	}
}

// A .gitignore with no trailing newline must not get the pattern glued onto
// its last entry.
func TestEnsureAttachGitignore_NoTrailingNewline(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(".gitignore", []byte("node_modules/"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ensureAttachGitignore(); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, ".gitignore")
	if strings.Contains(got, "node_modules/#") || strings.Contains(got, "node_modules/.shipmates") {
		t.Errorf("pattern glued onto the previous entry:\n%q", got)
	}
	if !gitignoreContainsPattern([]byte(got), attachInboxIgnorePattern) {
		t.Errorf("pattern not on a line of its own:\n%q", got)
	}
}

// ---------------------------------------------------------------------------
// mergeSessionStartHook — pure map surgery, worth pinning independently of
// the file I/O wrapper.
// ---------------------------------------------------------------------------

func TestMergeSessionStartHook(t *testing.T) {
	t.Run("adds to an empty settings map", func(t *testing.T) {
		s := map[string]any{}
		changed, err := mergeSessionStartHook(s, sessionStartHookCommand)
		if err != nil {
			t.Fatal(err)
		}
		if !changed {
			t.Error("changed = false on an empty map")
		}
		if countShipmatesHooks(s) != 1 {
			t.Errorf("hook not added: %+v", s)
		}
	})

	t.Run("second call reports no change", func(t *testing.T) {
		s := map[string]any{}
		if _, err := mergeSessionStartHook(s, sessionStartHookCommand); err != nil {
			t.Fatal(err)
		}
		changed, err := mergeSessionStartHook(s, sessionStartHookCommand)
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Error("changed = true on a re-merge; init/update would rewrite the file every run")
		}
		if countShipmatesHooks(s) != 1 {
			t.Errorf("duplicate hook entry: %+v", s)
		}
	})

	t.Run("survives a hooks key of the wrong type", func(t *testing.T) {
		// Someone hand-wrote "hooks": "nope". We must not panic; we replace
		// only the piece we own.
		s := map[string]any{"hooks": "nope", "keepMe": true}
		if _, err := mergeSessionStartHook(s, sessionStartHookCommand); err != nil {
			t.Fatal(err)
		}
		if countShipmatesHooks(s) != 1 {
			t.Errorf("hook not added over a malformed hooks key: %+v", s)
		}
		if s["keepMe"] != true {
			t.Error("unrelated key lost")
		}
	})

	t.Run("survives junk entries inside SessionStart", func(t *testing.T) {
		s := map[string]any{
			"hooks": map[string]any{
				"SessionStart": []any{"a string, not an object", 42, map[string]any{"hooks": "also wrong"}},
			},
		}
		if _, err := mergeSessionStartHook(s, sessionStartHookCommand); err != nil {
			t.Fatal(err)
		}
		if countShipmatesHooks(s) != 1 {
			t.Errorf("hook not added alongside junk: %+v", s)
		}
	})

	t.Run("other hook events are untouched", func(t *testing.T) {
		s := map[string]any{
			"hooks": map[string]any{
				"PreToolUse": []any{map[string]any{"hooks": []any{map[string]any{"type": "command", "command": "guard"}}}},
			},
		}
		if _, err := mergeSessionStartHook(s, sessionStartHookCommand); err != nil {
			t.Fatal(err)
		}
		hooks := s["hooks"].(map[string]any)
		if _, ok := hooks["PreToolUse"]; !ok {
			t.Error("PreToolUse hooks lost")
		}
	})
}

// The path the installer writes to must be the one Claude Code reads.
func TestClaudeSettingsPath(t *testing.T) {
	want := filepath.Join(".claude", "settings.json")
	if got := claudeSettingsPath(); got != want {
		t.Errorf("claudeSettingsPath() = %q, want %q", got, want)
	}
}
