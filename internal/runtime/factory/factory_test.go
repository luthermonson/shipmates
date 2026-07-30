package factory

import (
	"context"
	"errors"
	"os"
	goruntime "runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
)

// A codex construction attempt starts a real app-server transport, so the
// tests point its argv at a command that exits immediately. That exercises the
// wiring — argv plumbing, error classification — without needing codex
// installed, and without re-exec'ing the test binary (Windows locks a running
// executable, which leaves the test harness unable to clean it up).
func failingArgv() []string {
	if goruntime.GOOS == "windows" {
		return []string{"cmd", "/c", "exit", "1"}
	}
	return []string{"sh", "-c", "exit 1"}
}

// codexSettings is what these tests hand a codex selection: an argv that
// fails, and a startup timeout short enough that codexapp's own generous
// default (13s observed) does not dominate the package's runtime.
func codexSettings() map[string]any {
	return map[string]any{
		"command":            failingArgv(),
		"startup_timeout_ms": 1500,
	}
}

// --- containment ----------------------------------------------------------

func TestContainmentFor(t *testing.T) {
	cases := []struct {
		name       string
		in         config.Containment
		wantKind   string
		wantLimits containment.Limits
		wantErr    bool
	}{
		{
			name:     "empty defaults to watchdog with no caps",
			in:       config.Containment{},
			wantKind: "watchdog",
		},
		{
			name: "watchdog carries every limit through",
			in: config.Containment{
				Mode: "watchdog", MemoryLimitMB: 512, CPULimitSeconds: 60,
				MaxProcesses: 32, PollIntervalMS: 250, GracefulTimeoutMS: 1500,
			},
			wantKind: "watchdog",
			wantLimits: containment.Limits{
				MaxRSSBytes: 512 * 1024 * 1024, MaxCPUSeconds: 60, MaxProcesses: 32,
				PollInterval: 250 * time.Millisecond, GracefulTimeout: 1500 * time.Millisecond,
			},
		},
		{
			// "none" means unbounded. Handing back the limits alongside a
			// watcher that ignores them would be a confusing half-answer.
			name:     "none drops the limits with the watcher",
			in:       config.Containment{Mode: "none", MemoryLimitMB: 512, CPULimitSeconds: 60},
			wantKind: "none",
		},
		{
			name:     "cgroup degrades to watchdog but keeps the limits",
			in:       config.Containment{Mode: "cgroup", MemoryLimitMB: 256},
			wantKind: "watchdog",
			wantLimits: containment.Limits{
				MaxRSSBytes: 256 * 1024 * 1024,
			},
		},
		{
			name:    "unknown mode is an error",
			in:      config.Containment{Mode: "jail"},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, limits, err := containmentFor(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got watcher %v", w)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if w == nil {
				t.Fatal("watcher is nil; a nil watcher would silently mean unbounded")
			}
			if w.Kind() != tc.wantKind {
				t.Errorf("watcher kind = %q, want %q", w.Kind(), tc.wantKind)
			}
			if limits != tc.wantLimits {
				t.Errorf("limits = %+v, want %+v", limits, tc.wantLimits)
			}
		})
	}
}

// MaxProcesses is an int64 in config and a uint32 in Limits. An operator's
// absurd value must saturate, not wrap around to a tiny cap.
func TestContainmentFor_MaxProcessesSaturates(t *testing.T) {
	_, limits, err := containmentFor(config.Containment{MaxProcesses: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	if limits.MaxProcesses != ^uint32(0) {
		t.Errorf("MaxProcesses = %d, want saturation at %d", limits.MaxProcesses, ^uint32(0))
	}
}

// --- New ------------------------------------------------------------------

func TestNew_RequiresProjectDir(t *testing.T) {
	_, err := New(context.Background(), resolved("claude", nil), Options{})
	if err == nil {
		t.Fatal("expected an error without a ProjectDir")
	}
}

func TestNew_UnknownRuntime(t *testing.T) {
	_, err := New(context.Background(), resolved("gpt5", nil), Options{ProjectDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "unknown runtime") {
		t.Fatalf("err = %v, want an unknown-runtime error", err)
	}
}

func TestNew_UnknownContainmentMode(t *testing.T) {
	r := resolved("claude", nil)
	r.Containment.Mode = "jail"
	if _, err := New(context.Background(), r, Options{ProjectDir: t.TempDir()}); err == nil {
		t.Fatal("expected an error for an unknown containment mode")
	}
}

func TestNew_Claude(t *testing.T) {
	rt, err := New(context.Background(), resolved("claude", nil), Options{ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("claude should construct without a transport: %v", err)
	}
	defer rt.Close(context.Background())
	if rt.Name() != "claude" {
		t.Errorf("Name() = %q, want claude", rt.Name())
	}
}

// The operator's binary and argument vector must reach the runtime. They can
// only come from user config (see internal/runtime/config), which is what
// makes handing them to os/exec safe.
func TestNew_ClaudeHonorsOperatorBinaryAndArgs(t *testing.T) {
	settings := map[string]any{
		"binary":       "/opt/claude/bin/claude",
		"default_args": []any{"--model", "opus"},
	}
	rt, err := New(context.Background(), resolved("claude", settings), Options{ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())
	if rt.Name() != "claude" {
		t.Errorf("Name() = %q", rt.Name())
	}
}

// An openai selection with no settings cannot work — there is no base URL and
// no model — and the operator needs to be told that, not handed a bug report.
func TestNew_OpenAIWithoutSettingsIsNotConfigured(t *testing.T) {
	_, err := New(context.Background(), resolved("openai", nil), Options{ProjectDir: t.TempDir()})
	var notConfigured *runtime.ErrNotConfigured
	if !errors.As(err, &notConfigured) {
		t.Fatalf("err = %v (%T), want *runtime.ErrNotConfigured", err, err)
	}
	if notConfigured.Runtime != "openai" {
		t.Errorf("ErrNotConfigured.Runtime = %q, want openai", notConfigured.Runtime)
	}
}

func TestNew_OpenAIWithSettings(t *testing.T) {
	settings := map[string]any{
		"base_url": "http://127.0.0.1:8000/v1",
		"model":    "qwen3-coder",
	}
	rt, err := New(context.Background(), resolved("openai", settings), Options{ProjectDir: t.TempDir()})
	if err != nil {
		t.Fatalf("openai should construct from settings alone: %v", err)
	}
	defer rt.Close(context.Background())
	if rt.Name() != "openai" {
		t.Errorf("Name() = %q, want openai", rt.Name())
	}
}

// Codex needs a transport, so a failure to start one is an environment
// problem, not a bug. Classifying it as ErrNotConfigured is what lets a
// command print something actionable.
func TestNew_CodexTransportFailureIsNotConfigured(t *testing.T) {
	r := resolved("codex", codexSettings())
	_, err := New(context.Background(), r, Options{ProjectDir: t.TempDir()})
	if err == nil {
		t.Fatal("expected the bogus transport to fail")
	}
	var notConfigured *runtime.ErrNotConfigured
	if !errors.As(err, &notConfigured) {
		t.Fatalf("err = %v (%T), want *runtime.ErrNotConfigured", err, err)
	}
}

// Every name config accepts must be wired here. The two lists drifting apart
// is how you get a runtime shipmates validates and then cannot build.
func TestNew_CoversEveryKnownRuntime(t *testing.T) {
	for _, name := range config.Known {
		t.Run(name, func(t *testing.T) {
			settings := map[string]any{}
			if name == "codex" {
				settings = codexSettings()
			}
			rt, err := New(context.Background(), resolved(name, settings), Options{ProjectDir: t.TempDir()})
			if rt != nil {
				defer rt.Close(context.Background())
			}
			if err != nil && strings.Contains(err.Error(), "not wired here") {
				t.Fatalf("%s is in config.Known but the factory does not build it: %v", name, err)
			}
		})
	}
}

func TestNames_MatchesConfigKnown(t *testing.T) {
	if !slices.Equal(Names(), config.Known) {
		t.Errorf("Names() = %v, want %v", Names(), config.Known)
	}
	// And it must be a copy: a caller sorting the result must not reorder the
	// package-level list everyone else validates against.
	got := Names()
	got[0] = "mutated"
	if config.Known[0] == "mutated" {
		t.Error("Names() aliases config.Known")
	}
}

// --- Registry -------------------------------------------------------------

func TestRegistry_ImplementsFactory(t *testing.T) {
	var reg runtime.Factory = Registry{Options: Options{ProjectDir: t.TempDir()}}
	if !slices.Equal(reg.Names(), config.Known) {
		t.Errorf("Names() = %v", reg.Names())
	}
	rt, err := reg.New(context.Background(), "claude", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close(context.Background())
	if rt.Name() != "claude" {
		t.Errorf("Name() = %q", rt.Name())
	}
}

// --- persona + memory-hook seams -----------------------------------------

func TestPersonaArtifactPath(t *testing.T) {
	path, ok := PersonaArtifactPath("claude", "captain")
	if !ok || path == "" {
		t.Errorf("claude should have a tracked persona artifact, got %q ok=%v", path, ok)
	}
	if !strings.Contains(path, "captain") {
		t.Errorf("path = %q, want the persona name in it", path)
	}
	for _, name := range []string{"codex", "openai", "nonesuch"} {
		if p, ok := PersonaArtifactPath(name, "captain"); ok {
			t.Errorf("%s reported a tracked persona artifact %q; only claude has one today", name, p)
		}
	}
}

func TestPersonaArtifact_RendersForClaude(t *testing.T) {
	path, content, ok, err := PersonaArtifact("claude", runtime.PersonaSpec{
		Name:        "captain",
		Description: "coordinates the crew",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !ok || path == "" || len(content) == 0 {
		t.Fatalf("expected a rendered artifact, got path=%q len=%d ok=%v", path, len(content), ok)
	}
	if !strings.Contains(string(content), "captain") {
		t.Errorf("rendered artifact does not mention the persona:\n%s", content)
	}
}

func TestPersonaArtifact_NoneForOtherRuntimes(t *testing.T) {
	for _, name := range []string{"codex", "openai", "nonesuch"} {
		path, content, ok, err := PersonaArtifact(name, runtime.PersonaSpec{Name: "captain"})
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if ok || path != "" || content != nil {
			t.Errorf("%s: expected no artifact, got path=%q len=%d", name, path, len(content))
		}
	}
}

// The memory-hook seam must write only the selected runtime's configuration: a
// codex-only project must not grow a .claude/ directory, and vice versa.
func TestInstallMemoryHook_WritesOnlyTheSelectedRuntimesFiles(t *testing.T) {
	for _, tc := range []struct {
		runtime   string
		wantFiles []string
	}{
		{"claude", []string{".claude/settings.json"}},
		{"codex", nil},
		{"openai", nil},
		{"nonesuch", nil},
	} {
		t.Run(tc.runtime, func(t *testing.T) {
			dir := t.TempDir()
			if err := InstallMemoryHook(tc.runtime, dir); err != nil {
				t.Fatalf("InstallMemoryHook: %v", err)
			}
			for _, want := range tc.wantFiles {
				if _, err := os.Stat(dir + "/" + want); err != nil {
					t.Errorf("expected %s to be written: %v", want, err)
				}
			}
			if tc.wantFiles == nil {
				entries, err := os.ReadDir(dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 0 {
					t.Errorf("%s wrote %d entries into a project that did not select it", tc.runtime, len(entries))
				}
			}
		})
	}
}

func TestInstallMemoryHook_Idempotent(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		if err := InstallMemoryHook("claude", dir); err != nil {
			t.Fatalf("call #%d: %v", i+1, err)
		}
	}
}

// --- settings coercion ----------------------------------------------------

func TestStringSliceSetting(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want []string
		ok   bool
	}{
		{"yaml sequence", []any{"a", "b"}, []string{"a", "b"}, true},
		{"native slice", []string{"a"}, []string{"a"}, true},
		{"absent", nil, nil, false},
		{"wrong type", "a b", nil, false},
		{"empty", []any{}, nil, false},
		// A vector with a non-string element is dropped whole: a
		// half-applied argument list is worse than none.
		{"mixed types", []any{"--model", 7}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := stringSliceSetting(map[string]any{"k": tc.in}, "k")
			if ok != tc.ok || !slices.Equal(got, tc.want) {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestStringSetting(t *testing.T) {
	if v, ok := stringSetting(map[string]any{"k": "v"}, "k"); !ok || v != "v" {
		t.Errorf("got (%q, %v)", v, ok)
	}
	// An empty string is "unset", not "override with nothing" — otherwise a
	// stray `binary: ""` would blank out the default binary name.
	if _, ok := stringSetting(map[string]any{"k": ""}, "k"); ok {
		t.Error("an empty string should not count as set")
	}
	if _, ok := stringSetting(map[string]any{"k": 7}, "k"); ok {
		t.Error("a non-string should not count as set")
	}
}

func TestDurationSetting(t *testing.T) {
	cases := []struct {
		name string
		in   any
		want time.Duration
		ok   bool
	}{
		{"yaml int", 1500, 1500 * time.Millisecond, true},
		{"int64", int64(250), 250 * time.Millisecond, true},
		{"json float", float64(2000), 2 * time.Second, true},
		{"absent", nil, 0, false},
		{"string", "1500", 0, false},
		// A zero or negative timeout is never what an operator meant, so it
		// reads as unset rather than as "give up immediately".
		{"zero", 0, 0, false},
		{"negative", -5, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := durationSetting(map[string]any{"k": tc.in}, "k")
			if got != tc.want || ok != tc.ok {
				t.Errorf("got (%v, %v), want (%v, %v)", got, ok, tc.want, tc.ok)
			}
		})
	}
}

// --- helpers --------------------------------------------------------------

func resolved(name string, settings map[string]any) config.Resolved {
	return config.Resolved{
		Runtime:     name,
		Settings:    settings,
		Containment: config.Containment{Mode: config.DefaultContainmentMode},
		Source:      "test",
	}
}
