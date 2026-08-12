package project

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestPathHelpers(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"MemoryDir", MemoryDir("captain"), filepath.Join(Dir, MemoryDirName, "captain")},
		{"CommandPath", CommandPath("ask"), filepath.Join(CommandsDir, "ask.md")},
		{"PoliciesDir", PoliciesDir(), filepath.Join(Dir, PoliciesDirName)},
		{"PolicyPath", PolicyPath("captain"), filepath.Join(Dir, PoliciesDirName, "captain.yaml")},
		{"ManifestPath", ManifestPath(), filepath.Join(Dir, ManifestName)},
		{"SessionsDir", SessionsDir(), filepath.Join(Dir, SessionsDirName)},
		{"PortFile", PortFile(), filepath.Join(Dir, SessionsDirName, "server.port")},
		{"PidFile", PidFile(), filepath.Join(Dir, SessionsDirName, "server.pid")},
		{"LogFile", LogFile(), filepath.Join(Dir, SessionsDirName, "server.log")},
		{"SessionMarker", SessionMarker("captain"), filepath.Join(Dir, SessionsDirName, "captain.session")},
		{"InstallIDPath", InstallIDPath(), filepath.Join(Dir, InstallIDName)},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %q, want %q", tt.name, tt.got, tt.want)
		}
	}
	// Policies live under .shipmates/, not .claude/ — they're consumed by the
	// shipmates evaluator, not by Claude Code.
	if strings.HasPrefix(filepath.ToSlash(PolicyPath("x")), ".claude/") {
		t.Errorf("PolicyPath = %q, must not live under .claude/", PolicyPath("x"))
	}
}

func TestRepoName(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "project-one")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if got := RepoName(); got != "project-one" {
		t.Fatalf("RepoName = %q, want project-one", got)
	}
}

func TestInstallIDGeneratesAndPersists(t *testing.T) {
	t.Chdir(t.TempDir())

	id, err := InstallID()
	if err != nil {
		t.Fatalf("InstallID: %v", err)
	}
	if !regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("InstallID = %q, not a v4 UUID", id)
	}

	// Stability is the whole point: it disambiguates two clones of one repo on
	// the same fleet, so a second read must not mint a new one.
	again, err := InstallID()
	if err != nil {
		t.Fatalf("InstallID (second): %v", err)
	}
	if again != id {
		t.Fatalf("InstallID changed between calls: %q then %q", id, again)
	}

	b, err := os.ReadFile(InstallIDPath())
	if err != nil {
		t.Fatalf("InstallID did not persist to %s: %v", InstallIDPath(), err)
	}
	if strings.TrimSpace(string(b)) != id {
		t.Fatalf("file holds %q, want %q", strings.TrimSpace(string(b)), id)
	}
}

func TestInstallIDRegeneratesFromBlankFile(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(InstallIDPath(), []byte("   \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := InstallID()
	if err != nil {
		t.Fatalf("InstallID: %v", err)
	}
	if strings.TrimSpace(id) == "" {
		t.Fatal("blank install-id file yielded a blank id")
	}
}

func TestInstallIDReadsExisting(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(InstallIDPath(), []byte("  fixed-id-value\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	id, err := InstallID()
	if err != nil {
		t.Fatalf("InstallID: %v", err)
	}
	if id != "fixed-id-value" {
		t.Fatalf("InstallID = %q, want the trimmed stored value", id)
	}
}

func TestIsFleetPersonaFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	tests := []struct {
		name string
		path string
		want bool
	}{
		{"missing file", filepath.Join(dir, "nope.md"), false},
		{"no frontmatter", write("plain.md", "just a body\n"), true},
		{"absent field", write("absent.md", "---\nmodel: opus\n---\nbody\n"), true},
		{"opted in", write("in.md", "---\nshipmatesPersona: true\n---\nbody\n"), true},
		{"opted out", write("out.md", "---\nshipmatesPersona: false\n---\nbody\n"), false},
		// A parse failure must not silently drop a real crew member.
		{"malformed frontmatter", write("bad.md", "---\n\tshipmatesPersona: [\n---\nbody\n"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsFleetPersonaFile(tt.path); got != tt.want {
				t.Fatalf("IsFleetPersonaFile(%s) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestCommandBacked(t *testing.T) {
	if (PersonaConfig{Backend: "command"}).CommandBacked() != true {
		t.Error(`Backend "command" should be command-backed`)
	}
	for _, b := range []string{"", "claude", "Command", "commandish"} {
		if (PersonaConfig{Backend: b}).CommandBacked() {
			t.Errorf("Backend %q wrongly reported command-backed", b)
		}
	}
}

func TestRoutingOptionsResolved(t *testing.T) {
	tru, fls := true, false
	tests := []struct {
		name         string
		o            RoutingOptions
		wantB, wantL bool
	}{
		// Absent means the fleet default: both on.
		{"absent", RoutingOptions{}, true, true},
		{"explicit false", RoutingOptions{Bylines: &fls, Labels: &fls}, false, false},
		{"explicit true", RoutingOptions{Bylines: &tru, Labels: &tru}, true, true},
		{"mixed", RoutingOptions{Bylines: &fls, Labels: &tru}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, l := tt.o.Resolved()
			if b != tt.wantB || l != tt.wantL {
				t.Fatalf("Resolved() = (%v, %v), want (%v, %v)", b, l, tt.wantB, tt.wantL)
			}
		})
	}
}

func TestFleetConfigToken(t *testing.T) {
	// A distinctive name so this can't collide with a real var in the
	// developer's shell. Values (not names) are what we assert on, because
	// os.Getenv name matching is case-insensitive on Windows and
	// case-sensitive elsewhere — asserting on that would be a platform test.
	const custom = "SHIPMATES_TEST_CUSTOM_TOKEN_ENV"
	t.Setenv(DefaultFleetTokenEnv, "  default-secret\n")
	t.Setenv(custom, "custom-secret")

	tests := []struct {
		name string
		cfg  FleetConfig
		want string
	}{
		{"default env var", FleetConfig{}, "default-secret"},
		{"blank TokenEnv falls back to default", FleetConfig{TokenEnv: "   "}, "default-secret"},
		{"named env var", FleetConfig{TokenEnv: custom}, "custom-secret"},
		{"TokenEnv name is trimmed", FleetConfig{TokenEnv: "  " + custom + "  "}, "custom-secret"},
		{"unset var yields empty", FleetConfig{TokenEnv: "SHIPMATES_TEST_DEFINITELY_UNSET"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.Token(); got != tt.want {
				t.Fatalf("Token() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadConfigMalformed(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.WriteFile(ConfigName, []byte("crew: [unclosed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("want a parse error")
	}
	if !strings.Contains(err.Error(), ConfigName) {
		t.Fatalf("error = %q, want it to name %s", err, ConfigName)
	}
	// SessionPrefix swallows the error and yields no prefix rather than
	// producing a garbage session name.
	if got := SessionPrefix(); got != "" {
		t.Fatalf("SessionPrefix on a broken config = %q, want empty", got)
	}
}

func TestLoadManifestCorruptJSON(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(); err == nil {
		t.Fatal("want an error for a corrupt manifest — silently treating it as empty would let update clobber user edits")
	}
}

func TestLoadManifestNullFilesMap(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(Dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ManifestPath(), []byte(`{"version":"1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest()
	if err != nil {
		t.Fatal(err)
	}
	if m.Files == nil {
		t.Fatal("Files is nil; callers assign into it without checking")
	}
	m.Files["x"] = "y" // must not panic
}

func TestParsePersonaFrontmatterShapes(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		model string
	}{
		{"no frontmatter", "just a body\n", ""},
		{"unterminated frontmatter", "---\nmodel: opus\n", ""},
		{"leading blank lines", "\n\n---\nmodel: opus\n---\nbody\n", "opus"},
		{"crlf line endings", "---\r\nmodel: opus\r\n---\r\nbody\r\n", "opus"},
		{"not at start", "preamble\n---\nmodel: opus\n---\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fm, err := parsePersonaFrontmatter([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parsePersonaFrontmatter: %v", err)
			}
			if fm.Model != tt.model {
				t.Fatalf("Model = %q, want %q", fm.Model, tt.model)
			}
		})
	}
}

func TestResolveRemoteControlShapes(t *testing.T) {
	tests := []struct {
		name        string
		frontmatter string
		want        string
	}{
		{"true means the session name", "remoteControl: true\n", "captain"},
		{"false means off", "remoteControl: false\n", ""},
		{"absent means off", "model: opus\n", ""},
		{"string is used verbatim", "remoteControl: my-handle\n", "my-handle"},
		{"string is trimmed", "remoteControl: \"  my-handle  \"\n", "my-handle"},
		// A non-scalar (map/list) isn't a handle — must not crash or leak yaml.
		{"mapping is off", "remoteControl:\n  host: x\n", ""},
		{"sequence is off", "remoteControl:\n  - x\n", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			writeAgent(t, "captain", tt.frontmatter)
			cfg, err := ResolvePersonaConfig("captain")
			if err != nil {
				t.Fatalf("ResolvePersonaConfig: %v", err)
			}
			if cfg.RemoteControl != tt.want {
				t.Fatalf("RemoteControl = %q, want %q", cfg.RemoteControl, tt.want)
			}
		})
	}
}

func TestResolvePersonaConfigMalformedFrontmatter(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(AgentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AgentPath("captain"), []byte("---\nmodel: [unclosed\n---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ResolvePersonaConfig("captain")
	if err == nil {
		t.Fatal("want a parse error naming the persona file")
	}
	if !strings.Contains(filepath.ToSlash(err.Error()), "captain.md") {
		t.Fatalf("error = %q, want it to name captain.md", err)
	}
}

func TestResolvePersonaConfigCommandBackend(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "aider", "backend: command\ncommand:\n  - aider\n  - --model\n  - gpt\n")
	cfg, err := ResolvePersonaConfig("aider")
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.CommandBacked() {
		t.Fatalf("Backend = %q, want command-backed", cfg.Backend)
	}
	want := []string{"aider", "--model", "gpt"}
	if len(cfg.Command) != len(want) {
		t.Fatalf("Command = %v, want %v", cfg.Command, want)
	}
	for i := range want {
		if cfg.Command[i] != want[i] {
			t.Fatalf("Command = %v, want %v", cfg.Command, want)
		}
	}
}

func TestResolvePersonaConfigCrewCommandOverride(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "aider", "backend: command\ncommand:\n  - aider\n")
	if err := os.WriteFile(ConfigName, []byte("crew:\n  aider:\n    command:\n      - opencode\n      - run\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolvePersonaConfig("aider")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Command) != 2 || cfg.Command[0] != "opencode" {
		t.Fatalf("Command = %v, want the crew override", cfg.Command)
	}
}

func TestResolvePersonaConfigEmptyOverridesDoNotClobber(t *testing.T) {
	t.Chdir(t.TempDir())
	writeAgent(t, "captain", "model: opus\neffort: high\nberth: auto\ncwd: some/dir\nbackend: claude\npermissions:\n  mode: plan\n")
	// A crew entry that mentions the persona but sets nothing must leave the
	// frontmatter intact — empty strings are "unset", not "clear it".
	if err := os.WriteFile(ConfigName, []byte("crew:\n  captain: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResolvePersonaConfig("captain")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "opus" || cfg.Effort != "high" || cfg.Berth != "auto" || cfg.CWD != "some/dir" || cfg.Mode != "plan" || cfg.Backend != "claude" {
		t.Fatalf("empty crew override clobbered frontmatter: %+v", cfg)
	}
}
