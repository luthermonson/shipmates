package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDespeak(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain prose untouched", "Aye, Admiral. Both captains are on watch.", "Aye, Admiral. Both captains are on watch."},
		{"strips bold markers", "**Aye**, Admiral.", "Aye, Admiral."},
		{"strips backticks", "Run `bd show abc` next.", "Run bd show abc next."},
		{"strips underscore emphasis", "__very__ busy", "very busy"},
		{"strips heading hashes", "## Fleet status\nAll quiet.", "Fleet status\nAll quiet."},
		{"strips bullet dashes", "- picard is idle\n- data is working", "picard is idle\ndata is working"},
		{"drops table separator rows", "| ship | status |\n|------|--------|\n| laptop | idle |",
			"ship, status\nlaptop, idle"},
		{"em-dash cells dropped from tables", "| laptop | — |", "laptop"},
		{"trims surrounding blank space", "\n\n  Aye, Admiral.  \n\n", "Aye, Admiral."},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := despeak(tc.in); got != tc.want {
				t.Errorf("despeak(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// despeak feeds a TTS engine; a stray pipe or asterisk gets read aloud. This
// asserts the property rather than an exact string.
func TestDespeak_LeavesNoSpokenPunctuationArtifacts(t *testing.T) {
	in := "**Status**\n\n| ship | mate | state |\n|---|---|---|\n| laptop | picard | `idle` |\n- all quiet"
	got := despeak(in)
	for _, bad := range []string{"**", "`", "|---", "__"} {
		if strings.Contains(got, bad) {
			t.Errorf("despeak left %q in the spoken text: %q", bad, got)
		}
	}
}

func TestFirstLine(t *testing.T) {
	cases := []struct{ in, want string }{
		{"single line", "single line"},
		{"first\nsecond\nthird", "first"},
		// trimmed as a whole first, then cut at the first newline
		{"  padded\nrest", "padded"},
		{"", ""},
		{"\n\nleading blanks then text\nmore", "leading blanks then text"},
	}
	for _, tc := range cases {
		if got := firstLine(tc.in); got != tc.want {
			t.Errorf("firstLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	long := strings.Repeat("x", 500)
	if got := firstLine(long); len(got) != 200 {
		t.Errorf("firstLine must cap at 200 chars, got %d", len(got))
	}
}

func TestLastUserMessage(t *testing.T) {
	msgs := []chatMessage{
		{Role: "system", Content: "you are the commodore"},
		{Role: "user", Content: "first order"},
		{Role: "assistant", Content: "aye"},
		{Role: "user", Content: "second order"},
		{Role: "assistant", Content: "aye again"},
	}
	if got := lastUserMessage(msgs); got != "second order" {
		t.Errorf("want newest user turn, got %q", got)
	}
	if got := lastUserMessage(nil); got != "" {
		t.Errorf("empty history should yield empty string, got %q", got)
	}
	if got := lastUserMessage([]chatMessage{{Role: "assistant", Content: "hi"}}); got != "" {
		t.Errorf("no user turn should yield empty string, got %q", got)
	}
}

func TestClaudeBrainReadResult(t *testing.T) {
	c := newClaudeBrain("haiku", "127.0.0.1:8443", "tok")

	out, _ := json.Marshal(claudeTurnResult{Result: "**Aye**, Admiral.", SessionID: "sess-1", TotalCost: 0.01})
	reply, err := c.readResult(out)
	if err != nil {
		t.Fatalf("readResult: %v", err)
	}
	if reply != "Aye, Admiral." {
		t.Errorf("reply should be despeaked, got %q", reply)
	}
	if c.sessionID != "sess-1" {
		t.Errorf("session id not captured for resume, got %q", c.sessionID)
	}

	// A later turn without a session id must not clobber the resumable one.
	out2, _ := json.Marshal(claudeTurnResult{Result: "still here"})
	if _, err := c.readResult(out2); err != nil {
		t.Fatalf("readResult: %v", err)
	}
	if c.sessionID != "sess-1" {
		t.Errorf("session id was clobbered by a result carrying none: %q", c.sessionID)
	}
}

func TestClaudeBrainReadResult_Errors(t *testing.T) {
	c := newClaudeBrain("", "", "")

	if _, err := c.readResult([]byte("not json at all")); err == nil {
		t.Error("non-JSON claude output must be an error")
	}

	out, _ := json.Marshal(claudeTurnResult{IsError: true, Result: "rate limited\nsecond line", SessionID: "sess-9"})
	_, err := c.readResult(out)
	if err == nil {
		t.Fatal("is_error output must be an error")
	}
	if !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("error should carry the claude message, got %v", err)
	}
	if strings.Contains(err.Error(), "second line") {
		t.Errorf("error should be first-line only, got %v", err)
	}
	// Even a failed turn's session id is worth keeping: the session exists.
	if c.sessionID != "sess-9" {
		t.Errorf("session id from an errored turn should still be recorded, got %q", c.sessionID)
	}
}

// childEnv must prepend the running binary's directory to the EXISTING PATH
// entry. Appending a second "PATH=" loses to Windows' canonical "Path=" and a
// stale `shipmates` elsewhere on the system wins — the exact bug the comment
// in claudebrain.go warns about. Runs on every OS.
func TestClaudeBrainChildEnv(t *testing.T) {
	c := newClaudeBrain("haiku", "127.0.0.1:8443", "shhh")
	env := c.childEnv()

	var pathVars []string
	var pathValue string
	fleetURL := ""
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		switch {
		case strings.EqualFold(k, "PATH"):
			pathVars = append(pathVars, k)
			pathValue = v
		case k == "SHIPMATES_FLEET_URL":
			fleetURL = v
		}
	}

	if len(pathVars) != 1 {
		t.Fatalf("child env must carry exactly one PATH-ish variable, got %v", pathVars)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	exeDir := filepath.Dir(exe)
	first, _, _ := strings.Cut(pathValue, string(os.PathListSeparator))
	if first != exeDir {
		t.Errorf("exe dir must come FIRST on PATH.\n first: %q\nexeDir: %q", first, exeDir)
	}
	if fleetURL != "http://127.0.0.1:8443" {
		t.Errorf("SHIPMATES_FLEET_URL = %q", fleetURL)
	}
}

// M7: the session's context is filled with ship feeds and GitHub-derived text,
// so anything in its environment is one prompt injection away from being
// echoed out through an allowed command. The fleet credential must not be
// there — not under its own name, and not as a loose value either.
func TestClaudeBrainChildEnv_CarriesNoFleetToken(t *testing.T) {
	// The fleet process itself is usually started with the token exported;
	// the child env is built from os.Environ(), so this is the realistic case.
	t.Setenv("SHIPMATES_FLEET_TOKEN", "inherited-secret")
	c := newClaudeBrain("haiku", "127.0.0.1:8443", "shhh")

	for _, kv := range c.childEnv() {
		k, v, _ := strings.Cut(kv, "=")
		if strings.EqualFold(k, "SHIPMATES_FLEET_TOKEN") {
			t.Errorf("the fleet token must not be in the child env, found %q", kv)
		}
		if v == "shhh" || v == "inherited-secret" {
			t.Errorf("the fleet token leaked into the child env as %q", kv)
		}
	}
}

// The credential reaches the session as a 0600 file it can name but not read
// with any tool it is allowed to run.
func TestClaudeBrainTokenFile(t *testing.T) {
	c := newClaudeBrain("haiku", "127.0.0.1:8443", "shhh")
	path, err := c.ensureTokenFile()
	if err != nil {
		t.Fatalf("ensureTokenFile: %v", err)
	}
	if path == "" {
		t.Fatal("a fleet with a token must materialize a credential file")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read token file: %v", err)
	}
	if string(raw) != "shhh" {
		t.Errorf("token file holds %q", raw)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows does not model the unix mode bits, so only assert where it means
	// something. The group/other bits are the whole point of the check.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode = %v, want 0600", info.Mode().Perm())
	}
	// Repeated calls reuse the same file rather than littering temp dirs.
	again, err := c.ensureTokenFile()
	if err != nil || again != path {
		t.Errorf("ensureTokenFile is not idempotent: %q vs %q (%v)", again, path, err)
	}

	c.close()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("close must remove the credential file, stat err = %v", err)
	}
}

// A token-less dev fleet has no credential to write and no flag to teach.
func TestClaudeBrainTokenFile_NoneWithoutAToken(t *testing.T) {
	c := newClaudeBrain("", "localhost:1", "")
	path, err := c.ensureTokenFile()
	if err != nil || path != "" {
		t.Fatalf("want no file for a token-less fleet, got %q (%v)", path, err)
	}
	if got := captainPrompt(path); strings.Contains(got, "--token-file") {
		t.Error("a token-less fleet must not teach a --token-file argument")
	}
	c.close() // must not panic
}

// M7: curl was a general-purpose egress primitive in a session whose context
// is attacker-influenceable. The `shipmates fleet` CLI covers the Commodore's
// whole job, so the tool list is exactly that and nothing else.
func TestAllowedBrainTools_NoEgressPrimitive(t *testing.T) {
	if allowedBrainTools != "Bash(shipmates fleet:*)" {
		t.Fatalf("the brain's tool surface changed: %q", allowedBrainTools)
	}

	// Assert on the argv the child is ACTUALLY launched with, not just on the
	// constant: widening the surface at the call site is exactly the mistake
	// this test exists to catch.
	c := newClaudeBrain("haiku", "127.0.0.1:8443", "shhh")
	c.sessionID = "sess-1"
	argv := c.args("/tmp/creds/fleet-token")

	var allowed string
	for i, a := range argv {
		if a == "--allowedTools" && i+1 < len(argv) {
			allowed = argv[i+1]
		}
	}
	if allowed == "" {
		t.Fatalf("no --allowedTools in argv: %q", argv)
	}
	for _, banned := range []string{"curl", "wget", "nc ", "Bash(*)", "WebFetch", "Read"} {
		if strings.Contains(allowed, banned) {
			t.Errorf("--allowedTools must not include %q, got %q", banned, allowed)
		}
	}

	// And the credential must never appear as an argument — argv is visible in
	// ps/Task Manager to every process on the host.
	for _, a := range argv {
		if strings.Contains(a, "shhh") {
			t.Errorf("the fleet token appears in the child argv: %q", a)
		}
	}
}

// The Commodore prompt rides every invocation; a resumed turn without it is
// generic Claude that has forgotten it commands a fleet.
func TestCaptainPromptCoversToolSurface(t *testing.T) {
	got := captainPrompt("/tmp/creds/fleet-token")
	for _, want := range []string{
		"Commodore", "Admiral",
		"shipmates fleet ls", "shipmates fleet status", "shipmates fleet tell",
		"shipmates fleet tail", "shipmates fleet pending", "shipmates fleet resolve",
		"shipmates fleet beads", "shipmates fleet dispatch",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("captainPrompt is missing %q — the session can't use that tool", want)
		}
	}
	// Every subcommand has to carry the credential argument, or the session
	// silently loses the ability to do its job.
	for _, sub := range []string{"ls", "status", "tell", "tail", "pending", "resolve", "beads", "dispatch"} {
		if !strings.Contains(got, "shipmates fleet "+sub+" --token-file /tmp/creds/fleet-token") {
			t.Errorf("`%s` is missing its --token-file argument in the prompt", sub)
		}
	}
	if strings.Contains(got, "{{AUTH}}") {
		t.Error("prompt template placeholder left unexpanded")
	}
	// And the session is told not to hand the credential to anyone.
	if !strings.Contains(got, "never print its contents") {
		t.Error("prompt should tell the session the file is a credential")
	}
}
