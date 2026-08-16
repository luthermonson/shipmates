package commands

import (
	"fmt"
	"os"
	"strings"

	"github.com/luthermonson/shipmates/internal/bridge"
)

// Terminal-safe printing for text that did not come from the operator.
//
// Everything the CLI shows from a ship — feed lines, pending permission
// prompts, bead titles, captain keys, a fleet error body — is agent- or
// GitHub-derived. It reaches the operator's real terminal with nothing between
// it and the escape-sequence parser, which is a different position from the
// TUI: inside the bridge's pane, agent bytes are confined to a virtual grid by
// internal/bridge/vt, and everything the bridge draws itself goes through
// bridge.Chrome. The CLI had neither.
//
// A feed line carrying \x1b[2J clears the operator's screen; an OSC 52 writes
// their clipboard; a bidi override makes `rm -rf /` render as something
// harmless in a pending-approval listing the operator is about to say yes to.
// So the same sanitizer the TUI uses is applied here, on the way to stdout.
//
// bridge.Chrome is the entry point; these helpers only decide how much
// structure survives:
//
//   - safeLine — one field on one line (a bead title, a captain key).
//   - safeBlock — a multi-line body, where line breaks are the whole point of
//     reading it (a feed tail, a pending listing). Newlines are preserved by
//     sanitizing line by line; everything else a terminal would act on is not.
//
// Neither imposes a length bound (max = 0): truncating an operator's feed would
// be a usability regression, and length is not the threat — control sequences
// are.

// safeLine sanitizes a single-line value for display. Whitespace runs collapse,
// so a value that smuggled newlines cannot break the column layout it sits in.
func safeLine(s string) string { return bridge.ChromeLine(s, 0) }

// safeBlock sanitizes a multi-line body, keeping the line structure and
// dropping everything that could act on the terminal. A trailing newline in the
// input is preserved so piped output still ends with one.
func safeBlock(b []byte) string {
	s := string(b)
	if s == "" {
		return ""
	}
	trailing := strings.HasSuffix(s, "\n")
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	for i, ln := range lines {
		lines[i] = bridge.Chrome(strings.TrimSuffix(ln, "\r"), 0)
	}
	out := strings.Join(lines, "\n")
	if trailing {
		out += "\n"
	}
	return out
}

// safeErr sanitizes a remote-supplied error body for embedding in an error
// string. Bounded, because an error message is a one-line diagnostic and a
// fleet that replies with a megabyte of HTML should not own the operator's
// scrollback.
func safeErr(b []byte) string { return bridge.ChromeLine(string(b), 512) }

// printRemote writes a sanitized remote-derived body to stdout. os.Stdout is
// looked up per call rather than captured, so tests that swap it still see the
// output.
func printRemote(b []byte) {
	_, _ = fmt.Fprint(os.Stdout, safeBlock(b))
}
