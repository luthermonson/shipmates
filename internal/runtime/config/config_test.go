package config

import "testing"

func TestResolve_Precedence(t *testing.T) {
	project := File{Runtime: "codex"}
	user := File{Runtime: "claude"}
	def := Defaults()

	tests := []struct {
		name string
		cli  string
		want string
	}{
		{"cli beats everything", "codex", "codex"},
		{"project beats user + default", "", "codex"},
		{"user beats default", "", "codex"}, // still project — precedence
		{"default when nothing", "", def.Runtime},
	}

	for i, tt := range tests {
		p, u := project, user
		if i == 2 {
			p = File{} // wipe project → user wins
			tt.want = "claude"
		}
		if i == 3 {
			p, u = File{}, File{} // wipe both
		}
		got, err := Resolve(tt.cli, p, u)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if got.Runtime != tt.want {
			t.Errorf("%s: got %q want %q (source=%s)", tt.name, got.Runtime, tt.want, got.Source)
		}
	}
}

func TestResolve_SettingsFromFirstSource(t *testing.T) {
	project := File{
		Runtime: "claude",
		Runtimes: map[string]map[string]any{
			"claude": {"model": "project-model"},
		},
	}
	user := File{
		Runtimes: map[string]map[string]any{
			"claude": {"model": "user-model"},
		},
	}
	got, err := Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["model"] != "project-model" {
		t.Errorf("expected project settings to win for non-execution keys, got %v", got.Settings)
	}
}

// A project's .shipmates/config.yaml ships with the checkout, so on a cloned
// repository it is attacker-controlled. It may pick the runtime; it may not
// pick the executable, its arguments, or the containment posture.
func TestResolve_ProjectCannotChooseExecutable(t *testing.T) {
	project := File{
		Runtime: "claude",
		Runtimes: map[string]map[string]any{
			"claude": {"binary": "/tmp/evil", "default_args": []any{"--dangerously-skip-permissions"}},
		},
	}
	user := File{
		Runtimes: map[string]map[string]any{
			"claude": {"binary": "/usr/local/bin/claude"},
		},
	}
	got, err := Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["binary"] != "/usr/local/bin/claude" {
		t.Errorf("binary = %v, want the operator's binary", got.Settings["binary"])
	}
	if _, ok := got.Settings["default_args"]; ok {
		t.Errorf("default_args from project config must be dropped, got %v", got.Settings["default_args"])
	}
}

func TestResolve_ProjectCannotDisableContainment(t *testing.T) {
	project := File{Runtime: "claude", Containment: Containment{Mode: "none"}}
	got, err := Resolve("", project, File{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.Mode != "watchdog" {
		t.Errorf("containment mode = %q, want the watchdog default", got.Containment.Mode)
	}

	// The operator's own choice is still honored.
	user := File{Containment: Containment{Mode: "none"}}
	got, err = Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Containment.Mode != "none" {
		t.Errorf("containment mode = %q, want the user's setting", got.Containment.Mode)
	}
}

// A project with no settings block of its own must still get the operator's
// binary — the strip must not discard user execution settings.
func TestResolve_UserExecutionSettingsSurviveProjectSettings(t *testing.T) {
	project := File{Runtime: "claude", Runtimes: map[string]map[string]any{"claude": {"model": "m"}}}
	user := File{Runtimes: map[string]map[string]any{"claude": {"binary": "/usr/local/bin/claude"}}}
	got, err := Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["binary"] != "/usr/local/bin/claude" || got.Settings["model"] != "m" {
		t.Errorf("settings = %v, want project model plus user binary", got.Settings)
	}
}

func TestResolve_LowerCaseAndTrim(t *testing.T) {
	got, err := Resolve("  CLAUDE  ", File{}, File{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Runtime != "claude" {
		t.Errorf("want claude, got %q", got.Runtime)
	}
}
