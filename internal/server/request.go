package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/personaname"
)

// Request hygiene for the captain's API: how big a body may be, what a path
// segment is allowed to contain before it reaches the filesystem, and what a
// free-text field may carry before it becomes argv.
//
// These rules live here rather than inline in each handler because all three
// used to be applied unevenly. /attach had a MaxBytesReader and nothing else
// did; internal/fleet validated the persona segment at its proxy edge and the
// captain — the process that actually opens the file — did not.

// Body limits. These are ceilings on hostile input, not budgets for real
// traffic: the largest legitimate JSON body the captain sees is a hook
// payload, whose tool_input carries the full text of a file the agent is
// writing. Everything else is a handful of short fields.
const (
	// maxJSONBody bounds the ordinary JSON endpoints (tell, resolve, events,
	// bead create/close/update, pty resize).
	maxJSONBody = int64(1 << 20) // 1 MiB

	// maxHookBody bounds POST /hook/{persona}/{event}. Claude Code sends the
	// whole tool_input, so a Write of a large generated file arrives here in
	// full and must not be rejected.
	maxHookBody = int64(8 << 20) // 8 MiB

	// maxPTYInputBody bounds a keystroke batch. A paste is the big case; 64
	// KiB is far more than a human types and more than any sane paste.
	maxPTYInputBody = int64(1 << 16) // 64 KiB
)

// decodeJSONBody caps the request body and decodes it. It reports whether the
// handler should continue; on failure it has already written the response.
//
// http.MaxBytesReader (rather than io.LimitReader) is deliberate: it makes the
// oversized case an error the handler can see and refuse, instead of silently
// truncating the body into something that happens to parse. It also stops the
// server reading from a client that is streaming gigabytes. This mirrors what
// handleAttach already did for multipart uploads.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, limit int64, dst any, badMsg string) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return false
		}
		http.Error(w, badMsg, http.StatusBadRequest)
		return false
	}
	return true
}

// pathPersona reads the {persona} wildcard and validates it, writing a 400 and
// reporting false when the name is not a legal persona name.
//
// This is the fix for the captain half of the traversal. ServeMux percent-
// decodes PathValue, so a request line of "/pty/..%2f..%2f..%2fetc/start"
// matches the "/pty/{persona}/start" pattern and hands the handler the decoded
// "../../../etc". That value flowed into project.AgentPath and
// project.MemoryDir (arbitrary-location reads of *.md / *.yaml),
// project.SessionMarker (an arbitrary-location file *write* carrying JSON the
// caller influences), the per-persona policy lookup in internal/permissions,
// and berth.Dir → `git worktree add`. The path helpers now refuse an illegal
// name themselves, but refusing at the door is what turns a silent I/O failure
// into a 400 the caller can understand.
func pathPersona(w http.ResponseWriter, r *http.Request) (string, bool) {
	persona := r.PathValue("persona")
	if !personaname.Valid(persona) {
		http.Error(w, "invalid persona name", http.StatusBadRequest)
		return "", false
	}
	return persona, true
}

// hookEventOK bounds the {event} wildcard on POST /hook/{persona}/{event}.
// It is not a path segment on our side, but it is concatenated into an event
// Type ("hook:"+event) that the fleet mirrors into SQLite and renders, so a
// caller does not get to put newlines or an essay in it. Claude Code's hook
// names are all short CamelCase identifiers.
func hookEventOK(event string) bool {
	if event == "" || len(event) > 64 {
		return false
	}
	for _, r := range event {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// safeText bounds a caller-supplied string that is about to become argv, or an
// event line other agents read: trim it, cap its length, and refuse anything
// that could reframe the text it lands in.
//
// The characters refused are not a shell-escaping concern — there is no shell
// on any of these paths, and every value rides flag=value form so a leading
// dash cannot become a flag. They are a *rendering* concern. These strings end
// up in the shared beads graph, in the fleet's terminal output, and in other
// agents' prompts, where a stray CR rewrites the line above it, an ESC starts
// a terminal control sequence, and U+202E reverses the visual order of
// everything after it. None of that belongs in a bead title.
//
// multiline governs whether newlines and tabs survive — a bead description is
// markdown and legitimately has both; a title, an assignee, or an external ref
// is one line by construction.
func safeText(v string, limit int, multiline bool) (string, bool) {
	v = strings.TrimSpace(v)
	if len(v) > limit {
		return "", false
	}
	if !utf8.ValidString(v) {
		return "", false
	}
	for _, r := range v {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		// Cc/Cf covers C0 and C1 controls, DEL, the bidi overrides, and the
		// zero-width joiners; Zl/Zp covers U+2028/U+2029, which are line
		// breaks to a JS consumer but not to Go.
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) {
			return "", false
		}
	}
	return v, true
}
