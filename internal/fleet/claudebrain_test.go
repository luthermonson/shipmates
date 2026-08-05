package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
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
	fleetURL, fleetToken := "", ""
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
		case k == "SHIPMATES_FLEET_TOKEN":
			fleetToken = v
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
	if fleetToken != "shhh" {
		t.Errorf("SHIPMATES_FLEET_TOKEN = %q", fleetToken)
	}
}

// A fleet started without a token still has to produce a well-formed env — an
// empty token is a valid dev configuration, not a reason to omit the variable.
func TestClaudeBrainChildEnv_EmptyTokenStillExported(t *testing.T) {
	c := newClaudeBrain("", "localhost:1", "")
	var found bool
	for _, kv := range c.childEnv() {
		if kv == "SHIPMATES_FLEET_TOKEN=" {
			found = true
		}
	}
	if !found {
		t.Error("SHIPMATES_FLEET_TOKEN must be exported even when empty")
	}
}

// The Commodore prompt rides every invocation; a resumed turn without it is
// generic Claude that has forgotten it commands a fleet.
func TestCaptainPromptCoversToolSurface(t *testing.T) {
	for _, want := range []string{
		"Commodore", "Admiral",
		"shipmates fleet ls", "shipmates fleet status", "shipmates fleet tell",
		"shipmates fleet tail", "shipmates fleet pending", "shipmates fleet resolve",
		"shipmates fleet beads", "shipmates fleet dispatch",
	} {
		if !strings.Contains(captainPrompt, want) {
			t.Errorf("captainPrompt is missing %q — the session can't use that tool", want)
		}
	}
}
