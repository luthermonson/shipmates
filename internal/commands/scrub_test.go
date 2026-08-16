package commands

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// hostileFeedLine is what a mate could emit into a feed after reading a GitHub
// issue that contains terminal escapes: clear the screen, colour text through
// an 8-bit C1 introducer, set the window title, write the clipboard, and
// reorder a command with a bidi override so it renders as something harmless.
const hostileFeedLine = "geordi: \x1b[2Jrunning \u009b31mtests\x1b]0;pwned\x07 " +
	"and \u202ehs.tpircs\x1b]52;c;ZXZpbA==\x07\n"

// assertInert is the property that matters: whatever we print must be
// incapable of doing anything to the operator's terminal. Newlines are the one
// control character allowed through — a feed without line breaks is unreadable,
// and \n cannot move the cursor anywhere a new line would not.
func assertInert(t *testing.T, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("output is not valid UTF-8: %q", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			continue
		}
		if s[i] < 0x20 || s[i] == 0x7f {
			t.Errorf("control byte %#x survived at offset %d: %q", s[i], i, s)
		}
	}
	for _, r := range s {
		if r >= 0x80 && r <= 0x9f {
			t.Errorf("C1 control %#x survived: %q", r, s)
		}
		switch r {
		case '\u202e', '\u202d', '\u200f', '\u200e':
			t.Errorf("bidi control %#x survived: %q", r, s)
		}
	}
}

func TestSafeBlock(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text is untouched", "all hands on deck", "all hands on deck"},
		{"line structure survives", "one\ntwo\nthree", "one\ntwo\nthree"},
		{"a trailing newline survives", "one\ntwo\n", "one\ntwo\n"},
		{"crlf is normalised", "one\r\ntwo\r\n", "one\ntwo\n"},
		{"erase display is removed whole", "a\x1b[2Jb", "ab"},
		{"cursor position is removed whole", "a\x1b[999;999Hb", "ab"},
		{"sgr colour is removed, text kept", "a\x1b[31mred\x1b[0mb", "aredb"},
		{"osc title is removed whole", "a\x1b]0;pwned\x07b", "ab"},
		{"osc 52 clipboard write is removed", "a\x1b]52;c;ZXZpbA==\x07b", "ab"},
		{"eight bit C1 csi introducer", "a\u009b2Jb", "ab"},
		{"eight bit C1 osc introducer", "a\u009d0;x\x07b", "ab"},
		{"bidi override is dropped", "rm \u202esctipt.sh", "rm sctipt.sh"},
		{"bare carriage return is dropped", "aaaa\rbb", "aaaabb"},
		{"tab is dropped", "a\tb", "ab"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := safeBlock([]byte(tc.in))
			if got != tc.want {
				t.Errorf("safeBlock(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertInert(t, got)
		})
	}
}

// safeLine is for values that must occupy exactly one column of one line: a
// smuggled newline must not break the layout it sits in.
func TestSafeLineCollapsesToOneLine(t *testing.T) {
	got := safeLine("bd-1\nonline   captain\u202e\x1b[2J")
	if strings.Contains(got, "\n") {
		t.Errorf("safeLine kept a newline: %q", got)
	}
	if got != "bd-1 online captain" {
		t.Errorf("safeLine = %q", got)
	}
	assertInert(t, got)
}

// safeErr bounds the remote body as well as scrubbing it — an error line is a
// diagnostic, not a place for a fleet to dump a megabyte of HTML.
func TestSafeErrIsScrubbedAndBounded(t *testing.T) {
	got := safeErr([]byte("\x1b[2J" + strings.Repeat("x", 4096)))
	if len([]rune(got)) > 512 {
		t.Errorf("safeErr returned %d cells, want <= 512", len([]rune(got)))
	}
	assertInert(t, got)
}

// ---------------------------------------------------------------------------
// L8 end to end: the CLI paths that print ship-supplied text.
// ---------------------------------------------------------------------------

func TestFleetTailScrubsTheFeed(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(hostileFeedLine))
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "tail", "--fleet", srv.URL, "cap-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, out)
	// The legible text must survive — scrubbing is not censoring.
	if !strings.Contains(out, "running") || !strings.Contains(out, "tests") {
		t.Errorf("the feed text was lost: %q", out)
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "pwned") {
		t.Errorf("escape payload survived: %q", out)
	}
}

func TestFleetPendingScrubsThePrompt(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":"p1","command":"` + "\x1b[2Jrm -rf \u202e/" + `"}]`))
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "pending", "--fleet", srv.URL, "cap-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, out)
}

// Bead titles and assignees are written by agents out of GitHub issue text.
func TestFleetBeadsScrubsTitlesAndAssignees(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"id":       "bd-1\x1b[2J",
			"title":    "fix the \u009b31mparser\x1b]0;pwned\x07",
			"status":   "open\x1b[1;1H",
			"assignee": "geordi\u202e",
		}})
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "beads", "--fleet", srv.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, out)
	if !strings.Contains(out, "parser") {
		t.Errorf("the bead title was lost: %q", out)
	}
}

// Captain keys come from the ship's own connect headers.
func TestFleetLsScrubsCaptainKeys(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"client_key": "repo\x1b[2J:captain\u202e",
			"connected":  true,
			"port":       8443,
		}})
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "ls", "--fleet", srv.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, out)
}

func TestFleetStatusScrubsMateRows(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"client_key": "repo:captain\x1b]0;pwned\x07",
			"persona":    "geordi\u009b2J",
			"status":     "busy\x1b[2J",
		}})
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "status", "--fleet", srv.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	assertInert(t, out)
}

// A non-2xx body is remote-derived too, and it lands in an error string that
// the CLI prints.
func TestFleetErrorBodyIsScrubbed(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("no such \x1b[2Jcaptain\u202e"))
	}))
	defer srv.Close()

	err := Fleet().Run(context.Background(), []string{"fleet", "ls", "--fleet", srv.URL})
	if err == nil {
		t.Fatal("expected an error on 404")
	}
	assertInert(t, err.Error())
	if !strings.Contains(err.Error(), "captain") {
		t.Errorf("err = %v, want the server's message to survive scrubbing", err)
	}
}
