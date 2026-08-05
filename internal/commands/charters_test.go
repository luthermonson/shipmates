package commands

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

func charterCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("---\nname: geordi\n---\n\n# Geordi\n")},
		"catalog/charters/drain.md": {Data: []byte(
			"You are {{.Persona}}. Ship at most {{.Cap}} issues.\nFlow: {{.RoutingFlow}}\n")},
		"catalog/charters/autonomous.md": {Data: []byte(
			"Captain: {{.Captain}}\nCrew: {{.CrewList}}\nCadence: {{.Cadence}}\nCap: {{.Cap}}\nRead: {{.RoutingRead}}\n")},
		"catalog/charters/broken.md":       {Data: []byte("{{.Unclosed\n")},
		"catalog/charters/missingfield.md": {Data: []byte("{{.NotAField}}\n")},
	})
}

// ---------------------------------------------------------------------------
// renderCharter
// ---------------------------------------------------------------------------

func TestRenderCharter(t *testing.T) {
	cat := charterCatalog()

	t.Run("substitutes template data", func(t *testing.T) {
		got, err := renderCharter(cat, "drain", map[string]any{
			"Persona": "geordi", "Cap": 3, "RoutingFlow": "claim then ship",
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"You are geordi", "at most 3 issues", "claim then ship"} {
			if !strings.Contains(got, want) {
				t.Errorf("charter missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("a missing charter is a clear error", func(t *testing.T) {
		_, err := renderCharter(cat, "nosuch", nil)
		if err == nil {
			t.Fatal("expected an error for a missing charter")
		}
		if !strings.Contains(err.Error(), "nosuch") {
			t.Errorf("error should name the charter: %v", err)
		}
	})

	t.Run("an unparseable charter is a parse error, not a panic", func(t *testing.T) {
		_, err := renderCharter(cat, "broken", nil)
		if err == nil {
			t.Fatal("expected a parse error")
		}
		if !strings.Contains(err.Error(), "parse charter") {
			t.Errorf("err = %v, want a parse-charter error", err)
		}
	})

	t.Run("an execution failure surfaces as a render error", func(t *testing.T) {
		// A charter referencing a field the caller's data doesn't have must
		// fail loudly rather than dispatching a persona with a hole in its
		// instructions.
		_, err := renderCharter(cat, "missingfield", struct{ Persona string }{"geordi"})
		if err == nil {
			t.Fatal("expected a render error")
		}
		if !strings.Contains(err.Error(), "render charter") {
			t.Errorf("err = %v, want a render-charter error", err)
		}
	})
}

// ---------------------------------------------------------------------------
// activeRouting / routingFlow / routingStateRead
// ---------------------------------------------------------------------------

func TestActiveRouting(t *testing.T) {
	t.Run("no config yields no routing", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if got := activeRouting(); got != "" {
			t.Errorf("activeRouting() = %q, want empty", got)
		}
	})

	t.Run("reads the configured layer", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := activeRouting(); got != "github" {
			t.Errorf("activeRouting() = %q, want github", got)
		}
	})

	t.Run("a malformed config degrades to no routing rather than erroring", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: [unclosed\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := activeRouting(); got != "" {
			t.Errorf("activeRouting() = %q, want empty on a broken config", got)
		}
	})
}

func TestRoutingFlowAndStateRead(t *testing.T) {
	t.Run("github routing gets the PR flow", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: github\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if flow := routingFlow(); !strings.Contains(flow, "Closes #N") {
			t.Errorf("github flow missing the PR convention: %q", flow)
		}
		if read := routingStateRead(); !strings.Contains(read, "gh issue list") {
			t.Errorf("github state read missing the gh queue: %q", read)
		}
	})

	t.Run("no routing gets the generic flow", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if flow := routingFlow(); !strings.Contains(flow, "standard claim") {
			t.Errorf("generic flow = %q", flow)
		}
		if read := routingStateRead(); !strings.Contains(read, "work queue") {
			t.Errorf("generic state read = %q", read)
		}
	})

	t.Run("an unknown routing layer falls back to generic", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.WriteFile(project.ConfigName, []byte("routing: gitlab\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if flow := routingFlow(); !strings.Contains(flow, "standard claim") {
			t.Errorf("unknown routing did not fall back: %q", flow)
		}
	})
}

// ---------------------------------------------------------------------------
// installedPersonas
// ---------------------------------------------------------------------------

func TestInstalledPersonas(t *testing.T) {
	t.Run("empty project", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := installedPersonas()
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("sorted, .md only, opt-outs excluded", func(t *testing.T) {
		t.Chdir(t.TempDir())
		mustWrite(t, project.AgentPath("worf"), "---\nname: worf\n---\n\n# Worf\n")
		mustWrite(t, project.AgentPath("data"), "---\nname: data\n---\n\n# Data\n")
		// No frontmatter at all still counts as a fleet persona.
		mustWrite(t, project.AgentPath("alyssa"), "# Alyssa\n")
		// Explicit opt-out: a project-Q&A subagent, not crew.
		mustWrite(t, project.AgentPath("helper"), "---\nname: helper\nshipmatesPersona: false\n---\n\n# Helper\n")
		// Non-markdown files in the agents dir are ignored.
		mustWrite(t, ".claude/agents/notes.txt", "scratch")

		got, err := installedPersonas()
		if err != nil {
			t.Fatal(err)
		}
		want := []string{"alyssa", "data", "worf"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("installedPersonas() = %v, want %v", got, want)
		}
	})
}

// ---------------------------------------------------------------------------
// autonomousCharter
// ---------------------------------------------------------------------------

func TestAutonomousCharter_ExcludesTheCaptainFromTheCrew(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, p := range []string{"captain", "geordi", "worf"} {
		mustWrite(t, project.AgentPath(p), "---\nname: "+p+"\n---\n\n# "+p+"\n")
	}
	got, err := autonomousCharter(charterCatalog(), "captain", "5min,10", 3)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "Crew: geordi, worf") {
		t.Errorf("crew list wrong (captain should be excluded):\n%s", got)
	}
	if !strings.Contains(got, "Captain: captain") {
		t.Errorf("captain not substituted:\n%s", got)
	}
	if !strings.Contains(got, "Cadence: 5min,10") || !strings.Contains(got, "Cap: 3") {
		t.Errorf("cadence/cap not substituted:\n%s", got)
	}
}

// A themed coordinator (captainPersona: picard) must be excluded from its own
// crew list just like the default captain is.
func TestAutonomousCharter_CustomCaptainName(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, p := range []string{"picard", "geordi"} {
		mustWrite(t, project.AgentPath(p), "---\nname: "+p+"\n---\n\n# "+p+"\n")
	}
	got, err := autonomousCharter(charterCatalog(), "picard", "5min", 1)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "Crew: picard") || strings.Contains(got, ", picard") {
		t.Errorf("custom captain appears in its own crew list:\n%s", got)
	}
	if !strings.Contains(got, "Crew: geordi") {
		t.Errorf("crew list wrong:\n%s", got)
	}
}

func TestAutonomousCommand_PrintsTheCharter(t *testing.T) {
	t.Chdir(t.TempDir())
	mustWrite(t, project.AgentPath("captain"), "---\nname: captain\n---\n\n# Captain\n")
	mustWrite(t, project.AgentPath("geordi"), "---\nname: geordi\n---\n\n# Geordi\n")

	var err error
	out := captureStdout(t, func() {
		err = Autonomous(charterCatalog()).Run(context.Background(),
			[]string{"autonomous", "--cadence", "1min", "--cap", "7"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Captain: captain") {
		t.Errorf("default captain not used:\n%s", out)
	}
	if !strings.Contains(out, "Cadence: 1min") || !strings.Contains(out, "Cap: 7") {
		t.Errorf("flags not threaded into the charter:\n%s", out)
	}
}

func TestAutonomousCommand_PersonaFlagOverridesCaptain(t *testing.T) {
	t.Chdir(t.TempDir())
	mustWrite(t, project.AgentPath("picard"), "---\nname: picard\n---\n\n# Picard\n")
	mustWrite(t, project.AgentPath("geordi"), "---\nname: geordi\n---\n\n# Geordi\n")

	var err error
	out := captureStdout(t, func() {
		err = Autonomous(charterCatalog()).Run(context.Background(),
			[]string{"autonomous", "--persona", "picard"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Captain: picard") {
		t.Errorf("--persona not honored:\n%s", out)
	}
}

// ---------------------------------------------------------------------------
// Argument validation for the dispatching commands. These paths must fail
// BEFORE any `claude` subprocess is spawned.
// ---------------------------------------------------------------------------

func TestDrainCommand_ArgErrors(t *testing.T) {
	cat := charterCatalog()

	t.Run("no persona is a usage error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := Drain(cat).Run(context.Background(), []string{"drain"})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})

	t.Run("an uninstalled persona is refused", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := Drain(cat).Run(context.Background(), []string{"drain", "nobody"})
		if err == nil {
			t.Fatal("expected an error for an uninstalled persona")
		}
		if !strings.Contains(err.Error(), "not installed") {
			t.Errorf("err = %v, want a 'not installed' error", err)
		}
	})
}

func TestDrainManyCommand_ArgErrors(t *testing.T) {
	cat := charterCatalog()

	t.Run("no personas and no --all is a usage error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := DrainMany(cat).Run(context.Background(), []string{"drain-many"})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})

	t.Run("--all with nothing installed is a usage error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := DrainMany(cat).Run(context.Background(), []string{"drain-many", "--all"})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})

	t.Run("every persona failing is reported as a whole-run failure", func(t *testing.T) {
		t.Chdir(t.TempDir())
		var err error
		out := captureStdout(t, func() {
			err = DrainMany(cat).Run(context.Background(), []string{"drain-many", "nobody", "alsonobody"})
		})
		if err == nil || !strings.Contains(err.Error(), "all 2 drains failed") {
			t.Fatalf("err = %v, want 'all 2 drains failed'", err)
		}
		// Per-persona headers keep concurrent output legible.
		for _, want := range []string{"==== nobody ====", "==== alsonobody ===="} {
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q:\n%s", want, out)
			}
		}
	})
}

func TestFanoutCommand_ArgErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", []string{"fanout"}},
		{"personas but no prompt", []string{"fanout", "a,b"}},
		{"empty persona list", []string{"fanout", ",,,", "do the thing"}},
		{"whitespace-only prompt", []string{"fanout", "a", "   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			err := Fanout().Run(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), "usage:") {
				t.Fatalf("args %v: err = %v, want a usage error", tc.args, err)
			}
		})
	}
}

func TestFanoutCommand_AllPersonasMissing(t *testing.T) {
	t.Chdir(t.TempDir())
	var err error
	out := captureStdout(t, func() {
		err = Fanout().Run(context.Background(), []string{"fanout", " a , b ", "do the thing"})
	})
	if err == nil || !strings.Contains(err.Error(), "all 2 personas failed") {
		t.Fatalf("err = %v, want 'all 2 personas failed'", err)
	}
	// Whitespace around the comma-separated names must be trimmed, not carried
	// into the persona lookup.
	if !strings.Contains(out, "==== a ====") || !strings.Contains(out, "==== b ====") {
		t.Errorf("persona names not trimmed:\n%s", out)
	}
}

// oneShotDelegate refuses an uninstalled persona without spawning anything.
func TestOneShotDelegate_UninstalledPersona(t *testing.T) {
	t.Chdir(t.TempDir())
	out, err := oneShotDelegate(context.Background(), "nobody", "hi")
	if err == nil {
		t.Fatal("expected an error for an uninstalled persona")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("err = %v, want a 'not installed' error", err)
	}
	if out != nil {
		t.Errorf("expected no output, got %q", out)
	}
}

// ---------------------------------------------------------------------------
// remove — argument validation
// ---------------------------------------------------------------------------

func TestRemoveCommand(t *testing.T) {
	t.Run("no persona is a usage error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := Remove().Run(context.Background(), []string{"remove"})
		if err == nil || !strings.Contains(err.Error(), "usage:") {
			t.Fatalf("err = %v, want a usage error", err)
		}
	})

	t.Run("an uninstalled persona is refused", func(t *testing.T) {
		t.Chdir(t.TempDir())
		err := Remove().Run(context.Background(), []string{"remove", "nobody"})
		if err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Fatalf("err = %v, want a 'not installed' error", err)
		}
	})

	t.Run("removes the agent file and preserves memory by default", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.MkdirAll(project.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		agent := project.AgentPath("geordi")
		memFile := project.MemoryDir("geordi") + string(os.PathSeparator) + "notes.md"
		mustWrite(t, agent, "---\nname: geordi\n---\n")
		mustWrite(t, memFile, "hard-won knowledge\n")
		m := &project.Manifest{Files: map[string]string{agent: "sha", memFile: "sha"}}
		if err := m.Save(); err != nil {
			t.Fatal(err)
		}

		if err := Remove().Run(context.Background(), []string{"remove", "geordi"}); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if _, err := os.Stat(agent); !os.IsNotExist(err) {
			t.Errorf("agent file not removed (err=%v)", err)
		}
		if got := readFile(t, memFile); got != "hard-won knowledge\n" {
			t.Errorf("memory destroyed without --purge: %q", got)
		}
		after, err := project.LoadManifest()
		if err != nil {
			t.Fatal(err)
		}
		if _, still := after.Files[agent]; still {
			t.Error("manifest still records the removed agent file")
		}
		if _, still := after.Files[memFile]; !still {
			t.Error("manifest dropped the preserved memory file")
		}
	})

	t.Run("--purge deletes memory and its manifest entries", func(t *testing.T) {
		t.Chdir(t.TempDir())
		if err := os.MkdirAll(project.Dir, 0o755); err != nil {
			t.Fatal(err)
		}
		agent := project.AgentPath("geordi")
		memDir := project.MemoryDir("geordi")
		memFile := memDir + string(os.PathSeparator) + "notes.md"
		mustWrite(t, agent, "---\nname: geordi\n---\n")
		mustWrite(t, memFile, "notes\n")
		m := &project.Manifest{Files: map[string]string{agent: "sha", memFile: "sha"}}
		if err := m.Save(); err != nil {
			t.Fatal(err)
		}

		if err := Remove().Run(context.Background(), []string{"remove", "--purge", "geordi"}); err != nil {
			t.Fatalf("remove --purge: %v", err)
		}
		if _, err := os.Stat(memDir); !os.IsNotExist(err) {
			t.Errorf("memory dir survived --purge (err=%v)", err)
		}
		after, err := project.LoadManifest()
		if err != nil {
			t.Fatal(err)
		}
		if len(after.Files) != 0 {
			t.Errorf("manifest still has entries after purge: %v", after.Files)
		}
	})
}

// ---------------------------------------------------------------------------
// allow / deny — argument validation
// ---------------------------------------------------------------------------

func TestPermissionCommands_MissingID(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := Allow().Run(context.Background(), []string{"allow"}); err == nil ||
		!strings.Contains(err.Error(), "usage:") {
		t.Errorf("allow with no id: err = %v, want a usage error", err)
	}
	if err := Deny().Run(context.Background(), []string{"deny"}); err == nil ||
		!strings.Contains(err.Error(), "usage:") {
		t.Errorf("deny with no id: err = %v, want a usage error", err)
	}
}
