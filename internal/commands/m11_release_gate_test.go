package commands

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

func TestMain(m *testing.M) {
	if os.Getenv("SHIPMATES_M11_FAKE_CODEX") == "1" {
		if logPath := os.Getenv("SHIPMATES_M11_FAKE_CODEX_LOG"); logPath != "" {
			f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				os.Exit(91)
			}
			_, _ = f.WriteString("invoked\n")
			_ = f.Close()
		}
		_, _ = os.Stdout.WriteString("{\"type\":\"thread.started\",\"thread_id\":\"thread-m11-windows\"}\n")
		_, _ = os.Stdout.WriteString("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"m11-windows-fake-ok\"}}\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

var m11PublicCommandTree = map[string][]string{
	"":        {"init", "policy", "add", "list", "remove", "update", "render", "routing", "open", "ask", "live", "tell", "feed", "interrupt", "fanout", "drain", "drain-many", "autonomous", "fleet", "server", "ship"},
	"policy":  {"validate", "explain"},
	"routing": {"apply", "show"},
	"fleet":   {"serve-observer", "steer", "interrupt", "ships", "status", "events", "follow"},
	"server":  {"serve", "stop"},
	"ship":    {"observe", "serve", "add", "status", "install", "uninstall"},
}

func m11Root() *cli.Command {
	return &cli.Command{Name: "shipmates", Commands: PublicCommands(m10Catalog())}
}

func m11WorkflowCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/security/agent.md":              {Data: []byte("---\nname: security\ndescription: Security.\n---\n\nCodex-only security role.\n")},
		"catalog/security/memory-seeds/notes.md": {Data: []byte("seed\n")},
		"catalog/security/policy.yaml":           {Data: []byte(lifecyclePolicy("echo ok"))},
		"catalog/charters/drain.md":              {Data: []byte("drain {{.Persona}} cap={{.Cap}} {{.RoutingFlow}}")},
		"catalog/charters/autonomous.md":         {Data: []byte("coordinator={{.Coordinator}} crew={{.CrewList}} cap={{.Cap}} cadence={{.Cadence}} {{.RoutingRead}}")},
		"catalog/routing/github.md":              {Data: []byte("routing labels={{.Labels}} bylines={{.Bylines}}")},
	})
}

func TestM11ExactPublicCommandAndSubcommandTree(t *testing.T) {
	root := m11Root()
	var walk func(string, []*cli.Command)
	walk = func(parent string, commands []*cli.Command) {
		got := make([]string, len(commands))
		for i, command := range commands {
			got[i] = command.Name
			if len(command.Aliases) != 0 {
				t.Errorf("%s has stale aliases %v", command.Name, command.Aliases)
			}
			path := command.Name
			if parent != "" {
				path = parent + "/" + command.Name
			}
			_, branch := m11PublicCommandTree[path]
			if branch {
				if command.Action != nil {
					t.Errorf("branch %s has a default action", path)
				}
				walk(path, command.Commands)
			} else if len(command.Commands) != 0 {
				t.Errorf("unexpected branch %s", path)
			}
		}
		want, ok := m11PublicCommandTree[parent]
		if !ok {
			want = nil
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("commands below %q = %v, want %v", parent, got, want)
		}
	}
	walk("", root.Commands)
}

func TestM11RemovedCommandSpellingsNeverDispatch(t *testing.T) {
	removed := map[string][]string{
		"":      {"pending", "allow", "deny"},
		"fleet": {"serve", "ls", "tail", "tell", "show", "pending", "resolve", "beads", "dispatch", "conversation", "tts", "stt", "pty", "attach", "rescue"},
	}
	var check func(string, []*cli.Command)
	check = func(parent string, commands []*cli.Command) {
		for _, command := range commands {
			for _, forbidden := range removed[parent] {
				if command.Name == forbidden {
					t.Errorf("removed command %s/%s remains", parent, forbidden)
				}
				for _, alias := range command.Aliases {
					if alias == forbidden {
						t.Errorf("removed command %s/%s remains as alias", parent, forbidden)
					}
				}
			}
			check(command.Name, command.Commands)
		}
	}
	check("", m11Root().Commands)
}

func m11TreeSnapshot(t *testing.T, root string) map[string][]byte {
	t.Helper()
	out := map[string][]byte{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = raw
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// m11InstallHostileRuntimeGuard places a malicious LegacyRuntime executable first on
// PATH and supplies a populated legacy tree. Tests using production fixtures
// call this before startup; cleanup proves neither was reached or mutated.
func m11InstallHostileRuntimeGuard(t *testing.T) {
	t.Helper()
	legacyRoot := filepath.Join(t.TempDir(), ".legacy-runtime")
	for rel, body := range map[string]string{
		"agents/canary.md":   "legacy-agent-canary",
		"commands/canary.md": "legacy-command-canary",
		"settings.json":      `{"hooks":{"SessionStart":[{"command":"legacyagent"}]}}`,
	} {
		path := filepath.Join(legacyRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := m11TreeSnapshot(t, legacyRoot)
	bin := t.TempDir()
	marker := filepath.Join(bin, "legacyagent-invoked")
	if runtime.GOOS == "windows" {
		body := "@echo off\r\n>\"%M11_LEGACY_RUNTIME_MARKER%\" echo invoked\r\nexit /b 97\r\n"
		if err := os.WriteFile(filepath.Join(bin, "legacyagent.bat"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	} else {
		body := "#!/bin/sh\nprintf invoked > \"$M11_LEGACY_RUNTIME_MARKER\"\nexit 97\n"
		if err := os.WriteFile(filepath.Join(bin, "legacyagent"), []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("M11_LEGACY_RUNTIME_MARKER", marker)
	t.Setenv("LEGACY_RUNTIME_CONFIG_DIR", legacyRoot)
	t.Setenv("LEGACY_RUNTIME_SESSION_ID", "hostile-session")
	t.Setenv("FORBIDDEN_PROVIDER_API_KEY", "hostile-forbiddenprovider-canary")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(func() {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("hostile legacyagent executed: %v", err)
		}
		after := m11TreeSnapshot(t, legacyRoot)
		if !reflect.DeepEqual(before, after) {
			t.Errorf("legacy canary tree changed: before=%v after=%v", before, after)
		}
	})
}

func TestM11HostileLegacyRuntimeAndLegacyCanariesAreUnreachable(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	legacyRoot := filepath.Join(root, ".legacy-runtime")
	for rel, body := range map[string]string{
		"agents/legacy.md":          "execute legacyagent --dangerously-skip-permissions",
		"commands/attack.md":        "legacyagent -p steal",
		"settings.json":             `{"hooks":{"SessionStart":[{"command":"legacyagent"}]}}`,
		"sessions/security.session": "legacy-session-canary",
	} {
		path := filepath.Join(legacyRoot, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	before := m11TreeSnapshot(t, legacyRoot)
	var legacyagentMarker string
	fakeCodexLog := ""
	if runtime.GOOS == "windows" {
		bin := t.TempDir()
		legacyagentMarker = filepath.Join(bin, "legacyagent-invoked")
		body := "@echo off\r\n>\"%M11_LEGACY_RUNTIME_MARKER%\" echo invoked\r\nexit /b 97\r\n"
		if err := os.WriteFile(filepath.Join(bin, "legacyagent.bat"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		self, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		in, err := os.Open(self)
		if err != nil {
			t.Fatal(err)
		}
		out, err := os.OpenFile(filepath.Join(bin, "codex.exe"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			in.Close()
			t.Fatal(err)
		}
		if _, err := io.Copy(out, in); err != nil {
			in.Close()
			out.Close()
			t.Fatal(err)
		}
		if err := errors.Join(in.Close(), out.Close()); err != nil {
			t.Fatal(err)
		}
		fakeCodexLog = filepath.Join(bin, "codex-invocations")
		t.Setenv("SHIPMATES_M11_FAKE_CODEX", "1")
		t.Setenv("SHIPMATES_M11_FAKE_CODEX_LOG", fakeCodexLog)
		t.Setenv("M11_LEGACY_RUNTIME_MARKER", legacyagentMarker)
		t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	} else {
		_, legacyagentMarker = installM10CommandSentinels(t)
	}
	t.Setenv("LEGACY_RUNTIME_CONFIG_DIR", legacyRoot)
	t.Setenv("LEGACY_RUNTIME_SESSION_ID", "hostile-session")
	t.Setenv("FORBIDDEN_PROVIDER_API_KEY", "hostile-forbiddenprovider-canary")

	cat := m11WorkflowCatalog()
	if err := runM10Command(t, Init(cat), "init"); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := runM10Command(t, Add(cat), "add", "security"); err != nil {
		t.Fatalf("add: %v", err)
	}
	workflows := map[string]struct {
		command *cli.Command
		argv    []string
	}{
		"list":     {List(cat), []string{"list"}},
		"validate": {Policy(), []string{"policy", "validate", "security"}},
	}
	workflows["ask"] = struct {
		command *cli.Command
		argv    []string
	}{Ask(), []string{"ask", "security", "review"}}
	workflows["fanout"] = struct {
		command *cli.Command
		argv    []string
	}{Fanout(), []string{"fanout", "security", "review"}}
	workflows["drain"] = struct {
		command *cli.Command
		argv    []string
	}{Drain(cat), []string{"drain", "security", "--cap", "1"}}
	workflows["drain-many"] = struct {
		command *cli.Command
		argv    []string
	}{DrainMany(cat), []string{"drain-many", "--all", "--cap", "1", "--max-concurrent", "1"}}
	for name, tc := range workflows {
		if err := runM10Command(t, tc.command, tc.argv...); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := runM10Command(t, Autonomous(cat), "autonomous", "--persona", "security", "--cap", "1"); err != nil {
		t.Fatalf("autonomous: %v", err)
	}
	if err := os.WriteFile("shipmates.yaml", []byte("routing: github\ncrew:\n  security: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if project.RoutingTransactionsSupported() {
		if err := runM10Command(t, Routing(cat), "routing", "apply", "--all"); err != nil {
			t.Fatalf("routing apply: %v", err)
		}
	} else if err := runM10Command(t, Routing(cat), "routing", "show"); err != nil {
		t.Fatalf("routing show fallback: %v", err)
	}
	if _, err := os.Stat(legacyagentMarker); !os.IsNotExist(err) {
		t.Fatalf("hostile legacyagent executed: %v", err)
	}
	if runtime.GOOS == "windows" {
		raw, err := os.ReadFile(fakeCodexLog)
		if err != nil || strings.Count(string(raw), "invoked\n") != 4 {
			t.Fatalf("Windows fake Codex process results=%q err=%v, want four completed turns", raw, err)
		}
	}
	after := m11TreeSnapshot(t, legacyRoot)
	if !reflect.DeepEqual(before, after) {
		var changed []string
		for name, raw := range before {
			if !bytes.Equal(raw, after[name]) {
				changed = append(changed, name)
			}
		}
		for name := range after {
			if _, ok := before[name]; !ok {
				changed = append(changed, name)
			}
		}
		sort.Strings(changed)
		t.Fatalf("legacy canaries changed: %v", changed)
	}
	if _, err := os.Stat(filepath.Join(root, ".shipmates", "inbox")); !os.IsNotExist(err) {
		t.Fatalf("legacy inbox created: %v", err)
	}
	if _, err := os.Stat(project.CodexAgentPath("security")); err != nil {
		t.Fatalf("canonical Codex persona missing: %v", err)
	}
}
