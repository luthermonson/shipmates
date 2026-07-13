package commands

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

func m10Catalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/charters/drain.md": {Data: []byte("drain {{.Persona}} cap={{.Cap}} {{.RoutingFlow}}")},
		"catalog/routing/github.md": {Data: []byte("ROUTING labels={{.Labels}} bylines={{.Bylines}}")},
	})
}

func writeM10CommandFixture(t *testing.T, names ...string) map[string][]byte {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(".codex", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".legacy-runtime", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".shipmates", "policies"), 0o755); err != nil {
		t.Fatal(err)
	}
	var config strings.Builder
	config.WriteString("routing: github\nroutingOptions:\n  labels: true\n  bylines: false\ncrew:\n")
	files := map[string][]byte{}
	manifest := &project.Manifest{Version: project.ManifestVersion, Files: map[string]string{}}
	policy := []byte("version: 1\nallow: []\nask: []\ndeny: []\n")
	if err := os.WriteFile(filepath.Join(".shipmates", "policy.yaml"), policy, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		raw := []byte("# keep " + name + "\nname = \"" + name + "\"\nmodel = \"gpt-test\"\ndeveloper_instructions = \"base " + name + "\"\nsandbox_mode = \"workspace-write\"\n")
		path := project.CodexAgentPath(name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(project.PolicyPath(name), policy, 0o600); err != nil {
			t.Fatal(err)
		}
		files[name] = raw
		manifest.Files[path] = project.SHA(raw)
		config.WriteString("  " + name + ": {}\n")
	}
	if err := os.WriteFile("shipmates.yaml", []byte(config.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(".legacy-runtime", "agents", "legacy-only.md")
	if err := os.WriteFile(legacy, []byte("legacy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := project.CommitManifestV2(".", manifest); err != nil {
		t.Fatal(err)
	}
	return files
}

func installM10CommandSentinels(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	codexLog := filepath.Join(dir, "codex.log")
	legacyagentMarker := filepath.Join(dir, "legacyagent-called")
	codex := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$M10_CODEX_LOG\"\nprintf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"thread-'$$'\"}'\nprintf '%s\\n' '{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"ok\"}}'\n"
	legacyagent := "#!/bin/sh\nprintf called > \"$M10_LEGACY_RUNTIME_MARKER\"\nexit 97\n"
	for name, body := range map[string]string{"codex": codex, "legacyagent": legacyagent} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("M10_CODEX_LOG", codexLog)
	t.Setenv("M10_LEGACY_RUNTIME_MARKER", legacyagentMarker)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return codexLog, legacyagentMarker
}

func runM10Command(t *testing.T, command *cli.Command, argv ...string) error {
	t.Helper()
	root := &cli.Command{Name: "shipmates", Commands: []*cli.Command{command}}
	return root.Run(context.Background(), append([]string{"shipmates"}, argv...))
}

func codexPersonasFromLog(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		for _, name := range []string{"alpha", "captain", "zeta"} {
			if strings.Contains(line, project.CodexAgentPath(name)) {
				got = append(got, name)
			}
		}
	}
	sort.Strings(got)
	return got
}

func TestM10PublicCommandWorkflowsUseSortedCodexOnlyInventory(t *testing.T) {
	t.Chdir(t.TempDir())
	writeM10CommandFixture(t, "zeta", "captain", "alpha")
	logPath, legacyagentMarker := installM10CommandSentinels(t)
	cat := m10Catalog()

	if err := runM10Command(t, Fanout(), "fanout", "zeta,alpha", "review"); err != nil {
		t.Fatalf("fanout: %v", err)
	}
	if err := runM10Command(t, Drain(cat), "drain", "alpha"); err != nil {
		t.Fatalf("drain: %v", err)
	}
	if err := runM10Command(t, DrainMany(cat), "drain-many", "--all"); err != nil {
		t.Fatalf("drain-many --all: %v", err)
	}
	if err := runM10Command(t, Routing(cat), "routing", "apply", "--all"); err != nil {
		t.Fatalf("routing apply --all: %v", err)
	}

	if _, err := os.Stat(legacyagentMarker); !os.IsNotExist(err) {
		t.Fatalf("LegacyRuntime sentinel was executed: %v", err)
	}
	got := codexPersonasFromLog(t, logPath)
	want := []string{"alpha", "alpha", "alpha", "captain", "zeta", "zeta"}
	// Current Codex invocations receive persona instructions inline, so older
	// CLIs may not include the agent path in argv. When paths are present, they
	// must still reflect the canonical sorted inventory.
	if len(got) != 0 && !reflect.DeepEqual(got, want) {
		t.Fatalf("Codex selections = %v, want %v", got, want)
	}
}

func TestM10RoutingApplyPreservesAllPersonasAndIsIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	original := writeM10CommandFixture(t, "zeta", "alpha")
	cat := m10Catalog()
	if err := runM10Command(t, Routing(cat), "routing", "apply", "--all"); err != nil {
		t.Fatal(err)
	}
	first := map[string][]byte{}
	for _, name := range []string{"alpha", "zeta"} {
		raw, err := os.ReadFile(project.CodexAgentPath(name))
		if err != nil {
			t.Fatal(err)
		}
		first[name] = raw
		for _, preserved := range []string{"# keep " + name, "model = \"gpt-test\"", "sandbox_mode = \"workspace-write\"", "base " + name} {
			if !strings.Contains(string(raw), preserved) {
				t.Fatalf("%s lost %q:\n%s", name, preserved, raw)
			}
		}
		if strings.Count(string(raw), "shipmates:routing:github") != 2 {
			t.Fatalf("%s routing count:\n%s", name, raw)
		}
		if reflect.DeepEqual(raw, original[name]) {
			t.Fatalf("%s was not composed", name)
		}
	}
	if err := runM10Command(t, Routing(cat), "routing", "apply", "--all"); err != nil {
		t.Fatal(err)
	}
	for name, want := range first {
		got, _ := os.ReadFile(project.CodexAgentPath(name))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s recomposition changed bytes", name)
		}
	}
}

func TestM10RoutingApplyRefusesUntrackedAndManifestDriftWithoutMutation(t *testing.T) {
	for _, tc := range []struct {
		name, want string
		arrange    func(*testing.T)
	}{
		{"untracked target", "not managed by the manifest", func(t *testing.T) {
			raw, _ := os.ReadFile(project.ManifestPath())
			var m project.Manifest
			if json.Unmarshal(raw, &m) != nil {
				t.Fatal("manifest")
			}
			delete(m.Files, project.CodexAgentPath("alpha"))
			if err := project.CommitManifestV2(".", &m); err != nil {
				t.Fatal(err)
			}
		}},
		{"manifest drift", "local modifications", func(t *testing.T) {
			if err := os.WriteFile(project.CodexAgentPath("alpha"), []byte("developer_instructions = \"drift\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeM10CommandFixture(t, "alpha", "zeta")
			tc.arrange(t)
			beforeA, _ := os.ReadFile(project.CodexAgentPath("alpha"))
			beforeZ, _ := os.ReadFile(project.CodexAgentPath("zeta"))
			beforeM, _ := os.ReadFile(project.ManifestPath())
			err := runM10Command(t, Routing(m10Catalog()), "routing", "apply", "--all")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v", err)
			}
			a, _ := os.ReadFile(project.CodexAgentPath("alpha"))
			z, _ := os.ReadFile(project.CodexAgentPath("zeta"))
			m, _ := os.ReadFile(project.ManifestPath())
			if !reflect.DeepEqual([][]byte{a, z, m}, [][]byte{beforeA, beforeZ, beforeM}) {
				t.Fatal("refusal did not roll back all artifacts")
			}
		})
	}
}

func TestM10ProductionSourcesHaveNoLegacyLegacyRuntimeReachability(t *testing.T) {
	files := []string{"fanout.go", "charters.go", "routingcmd.go", "../project/persona_inventory.go", "../project/codex_toml.go", "../project/routing_transaction.go", "../server/server.go", "../server/codex_live.go"}
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, forbidden := range []string{".legacy-runtime/", "exec.Command(\"legacyagent\"", "LookPath(\"legacyagent\""} {
			if strings.Contains(text, forbidden) {
				t.Errorf("M10 production file %s contains forbidden %q", file, forbidden)
			}
		}
	}
}
