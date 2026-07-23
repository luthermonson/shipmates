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
			"claude": {"binary": "/project/claude"},
		},
	}
	user := File{
		Runtimes: map[string]map[string]any{
			"claude": {"binary": "/user/claude"},
		},
	}
	got, err := Resolve("", project, user)
	if err != nil {
		t.Fatal(err)
	}
	if got.Settings["binary"] != "/project/claude" {
		t.Errorf("expected project settings to win, got %v", got.Settings)
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
