package doctor

import (
	"bytes"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// --- shared test helpers ----------------------------------------------------

// isolate points project-relative reads at a fresh temp project (via t.Chdir)
// and the operator-file home at a sibling dir (via HOME/USERPROFILE and the
// returned home), so no check touches the real machine's state.
func isolate(t *testing.T) (proj, home string) {
	t.Helper()
	root := t.TempDir()
	proj = filepath.Join(root, "proj")
	home = filepath.Join(root, "home")
	for _, d := range []string{proj, home} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(proj)
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
	return proj, home
}

// lookPathFor returns a LookPath seam where only the named binaries resolve.
func lookPathFor(present ...string) func(string) (string, error) {
	set := map[string]bool{}
	for _, p := range present {
		set[p] = true
	}
	return func(name string) (string, error) {
		if set[name] {
			return "/usr/bin/" + name, nil
		}
		return "", exec.ErrNotFound
	}
}

// getenvFrom returns a Getenv seam backed by a map.
func getenvFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// baseEnv is a fully-wired Env whose probes report the quiet, healthy defaults;
// individual tests override the fields they exercise.
func baseEnv(home string) Env {
	return Env{
		Home:        home,
		GOOS:        "linux",
		LookPath:    lookPathFor("claude", "git"),
		Getenv:      func(string) string { return "" },
		HealthProbe: func(int, string) bool { return false },
		Supervisor:  func(string) SupervisorProbe { return SupervisorProbe{Supported: false} },
	}
}

// findResult returns the first result with the given name.
func findResult(t *testing.T, results []Result, name string) Result {
	t.Helper()
	for _, r := range results {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no result named %q in %+v", name, results)
	return Result{}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- Summarize / Render -----------------------------------------------------

func TestSummarizeAndFailed(t *testing.T) {
	results := []Result{
		{Status: OK}, {Status: OK}, {Status: Warn}, {Status: Fail},
	}
	s := Summarize(results)
	if s.OK != 2 || s.Warn != 1 || s.Fail != 1 {
		t.Fatalf("bad summary: %+v", s)
	}
	if !s.Failed() {
		t.Fatal("expected Failed() true when a Fail is present")
	}
	if Summarize([]Result{{Status: OK}, {Status: Warn}}).Failed() {
		t.Fatal("Warn must not make Failed() true")
	}
}

func TestRenderGroupsAndSummary(t *testing.T) {
	results := []Result{
		{Name: "claude CLI", Group: "Toolchain", Status: OK, Detail: "found"},
		{Name: "fleet url", Group: "Fleet", Status: Fail, Detail: "plaintext", Hint: "use https"},
		{Name: "git", Group: "Toolchain", Status: Warn, Detail: "missing"},
	}
	var buf bytes.Buffer
	s := Render(&buf, results)
	out := buf.String()

	if !strings.Contains(out, "\nToolchain\n") || !strings.Contains(out, "\nFleet\n") {
		t.Fatalf("missing group headers:\n%s", out)
	}
	// Toolchain must render before Fleet (groupOrder).
	if strings.Index(out, "Toolchain") > strings.Index(out, "Fleet") {
		t.Fatalf("groups out of order:\n%s", out)
	}
	for _, want := range []string{"[ OK ]", "[WARN]", "[FAIL]", "hint: use https", "summary: 1 OK, 1 WARN, 1 FAIL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if s.Fail != 1 {
		t.Fatalf("summary Fail = %d, want 1", s.Fail)
	}
}

func TestRenderIncludesUnknownGroup(t *testing.T) {
	var buf bytes.Buffer
	Render(&buf, []Result{{Name: "x", Group: "Zzz", Status: OK}})
	if !strings.Contains(buf.String(), "Zzz") {
		t.Fatalf("unexpected group was dropped:\n%s", buf.String())
	}
}

// --- sessionFilePerm (pure) -------------------------------------------------

func TestSessionFilePerm(t *testing.T) {
	tests := []struct {
		name string
		goos string
		mode fs.FileMode
		want Status
	}{
		{"unix 0600 ok", "linux", 0o600, OK},
		{"unix 0644 loose", "linux", 0o644, Warn},
		{"unix 0640 loose", "linux", 0o640, Warn},
		{"windows caveat regardless", "windows", 0o666, OK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := sessionFilePerm("server.token", tc.goos, tc.mode)
			if r.Status != tc.want {
				t.Fatalf("status = %v, want %v (detail: %s)", r.Status, tc.want, r.Detail)
			}
			if tc.goos == "windows" && !strings.Contains(r.Detail, "DACL") {
				t.Fatalf("windows result should state the DACL caveat: %s", r.Detail)
			}
		})
	}
}

// --- supervisor parsers (pure) ----------------------------------------------

func TestParseSupervisor(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		probe  SupervisorProbe
		want   Status
		detail string // substring
	}{
		{"unsupported os", "plan9", SupervisorProbe{Supported: false}, OK, "unknown"},
		{"systemd enabled", "linux", SupervisorProbe{Supported: true, Out: "enabled\n"}, OK, "enabled"},
		{"systemd disabled", "linux", SupervisorProbe{Supported: true, Out: "disabled\n", Err: true}, Warn, "disabled"},
		// N2: masked and enabled-runtime must not fall through to a false "not
		// installed" OK; a genuinely unrecognized state must be an honest Warn.
		{"systemd masked", "linux", SupervisorProbe{Supported: true, Out: "masked\n", Err: true}, Warn, "masked"},
		{"systemd enabled-runtime", "linux", SupervisorProbe{Supported: true, Out: "enabled-runtime\n"}, Warn, "enabled-runtime"},
		{"systemd unknown state", "linux", SupervisorProbe{Supported: true, Out: "some-future-state\n", Err: true}, Warn, "unrecognized"},
		{"systemd not installed", "linux", SupervisorProbe{Supported: true, Out: "Failed to get unit file state: No such file or directory\n", Err: true}, OK, "not installed"},
		{"launchd loaded", "darwin", SupervisorProbe{Supported: true, Out: "PID\tStatus\tLabel\n1\t0\tcc.shipmates.ship\n"}, OK, "loaded"},
		{"launchd not loaded", "darwin", SupervisorProbe{Supported: true, Out: "PID\tStatus\tLabel\n1\t0\tcom.apple.foo\n"}, OK, "not loaded"},
		// N3: schtasks is parsed from the verbose CSV "Scheduled Task State"
		// column, not a substring scan of localized LIST text.
		{"schtasks installed enabled", "windows", SupervisorProbe{Supported: true, Out: "\"TaskName\",\"Status\",\"Scheduled Task State\"\n\"\\ShipmatesShip\",\"Ready\",\"Enabled\"\n"}, OK, "installed"},
		{"schtasks disabled", "windows", SupervisorProbe{Supported: true, Out: "\"TaskName\",\"Status\",\"Scheduled Task State\"\n\"\\ShipmatesShip\",\"Ready\",\"Disabled\"\n"}, Warn, "disabled"},
		{"schtasks disabled localized stays honest", "windows", SupervisorProbe{Supported: true, Out: "\"TaskName\",\"Status\",\"Scheduled Task State\"\n\"\\ShipmatesShip\",\"Bereit\",\"Deaktiviert\"\n"}, OK, "locale"},
		{"schtasks not installed", "windows", SupervisorProbe{Supported: true, Out: "ERROR: The system cannot find the file specified.\n", Err: true}, OK, "not installed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := parseSupervisor(tc.goos, tc.probe)
			if r.Status != tc.want {
				t.Fatalf("status = %v, want %v (detail: %s)", r.Status, tc.want, r.Detail)
			}
			if !strings.Contains(strings.ToLower(r.Detail), strings.ToLower(tc.detail)) {
				t.Fatalf("detail %q missing %q", r.Detail, tc.detail)
			}
		})
	}
}
