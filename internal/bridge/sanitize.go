package bridge

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// Chrome sanitizes a string for rendering OUTSIDE the terminal pane — tab
// labels, persona names, status text, approval tool names and command
// summaries, error messages.
//
// This is the trust boundary for everything the bridge draws itself. Inside the
// pane, agent bytes are interpreted by internal/bridge/vt, which confines them
// to a virtual grid. Outside the pane there is no such confinement: a persona
// name read from /status.json or a Bash command echoed in an approval row goes
// into a string that lands on the operator's real terminal. So:
//
//   - a complete escape sequence is removed as a UNIT — introducer, parameters
//     and final byte. Dropping only the introducer would already be safe (an
//     escape sequence with no ESC is inert text), but it leaves the payload on
//     screen as garbage: "ESC [ 2 J" would render as "[2J", and an OSC title set
//     would leave "0;whatever" sitting in a tab label. Worse, that residue is
//     attacker-chosen text, so an agent could use it to paint something that
//     reads like the bridge's own chrome. Both 7-bit (ESC-prefixed) and 8-bit
//     (U+009B CSI, U+009D OSC, U+0090 DCS …) forms are recognised;
//   - every remaining C0 control (including CR, LF, TAB, BEL) is dropped;
//   - C1 controls (U+0080–U+009F) are dropped;
//   - Unicode bidi controls and other format characters (category Cf) are
//     dropped, so a right-to-left override cannot reorder a rendered command to
//     read as something benign;
//   - unpaired surrogates and invalid UTF-8 become U+FFFD;
//   - the result is truncated to max display cells with a one-cell ellipsis, so
//     an over-long value cannot push the rest of a line off screen or wrap the
//     layout.
//
// max <= 0 means "no length bound" (still fully sanitized).
func Chrome(s string, max int) string {
	// Scrub first, measure second. Deciding where to cut while still scanning for
	// escape sequences is what produced an earlier off-by-one, where the ellipsis
	// was appended *past* max and a 24-cell budget rendered 25 cells.
	kept := make([]rune, 0, len(s))
	widths := make([]int, 0, len(s))
	total := 0
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if n := controlSeqLen(s[i:], r, size); n > 0 {
			i += n
			continue
		}
		i += size
		if !allowedInChrome(r) {
			continue
		}
		if r == utf8.RuneError {
			// DecodeRune yields RuneError for invalid bytes; keep it visible as
			// a replacement char rather than silently dropping data.
			r = '�'
		}
		w := runewidth.RuneWidth(r)
		if w == 0 {
			w = 1 // count zero-width runes so a pile of them can't blow the bound
		}
		kept = append(kept, r)
		widths = append(widths, w)
		total += w
	}
	if max <= 0 || total <= max {
		return string(kept)
	}
	// Truncating: the ellipsis has to fit INSIDE the budget, so the text gets
	// max-1 cells. A budget of one cell is the ellipsis and nothing else.
	budget := max - 1
	var b strings.Builder
	used := 0
	for i, r := range kept {
		if used+widths[i] > budget {
			break
		}
		used += widths[i]
		b.WriteRune(r)
	}
	b.WriteRune('…')
	return b.String()
}

// controlSeqLen reports how many bytes at the start of s form one complete
// escape/control sequence, or 0 if s does not begin one. r/size are the rune
// already decoded at s[0], so the 8-bit introducers work whether they arrived as
// raw bytes or as UTF-8-encoded C1 code points.
//
// This is a scrubber, not an emulator: it only needs to find where a sequence
// ends so the whole thing can be discarded. When a sequence is unterminated the
// remainder of the string is consumed, which is deliberate — an unterminated OSC
// would swallow the rest of the line on a real terminal too, so dropping it is
// both safe and honest. Legibility of hostile input is not a goal.
func controlSeqLen(s string, r rune, size int) int {
	switch r {
	case 0x1b: // ESC
		if size >= len(s) {
			return len(s) // lone trailing ESC
		}
		switch c := s[size]; {
		case c == '[': // CSI
			return size + 1 + csiTailLen(s[size+1:])
		case c == ']', c == 'P', c == 'X', c == '^', c == '_':
			// OSC, DCS, SOS, PM, APC — all string sequences ended by ST/BEL.
			return size + 1 + stTailLen(s[size+1:])
		case c == '(', c == ')', c == '*', c == '+', c == '-', c == '.',
			c == '/', c == '%', c == '#', c == ' ':
			// Charset designators and similar: one intermediate plus one final.
			if size+2 <= len(s) {
				return size + 2
			}
			return len(s)
		default:
			return size + 1 // two-byte escape: ESC c, ESC =, ESC 7, ESC \, …
		}
	case 0x9b: // 8-bit CSI
		return size + csiTailLen(s[size:])
	case 0x9d, 0x90, 0x98, 0x9e, 0x9f: // 8-bit OSC, DCS, SOS, PM, APC
		return size + stTailLen(s[size:])
	}
	return 0
}

// csiTailLen measures a CSI body: parameter and intermediate bytes 0x20–0x3F,
// then one final byte 0x40–0x7E. A malformed body consumes only what matched,
// leaving the rest to be sanitized rune by rune.
func csiTailLen(s string) int {
	i := 0
	for i < len(s) && s[i] >= 0x20 && s[i] <= 0x3f {
		i++
	}
	if i < len(s) && s[i] >= 0x40 && s[i] <= 0x7e {
		i++
	}
	return i
}

// stTailLen measures a string-sequence body up to and including its terminator:
// BEL, 7-bit ST (ESC \), or 8-bit ST (U+009C, raw or UTF-8 encoded).
func stTailLen(s string) int {
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == 0x07: // BEL
			return i + 1
		case s[i] == 0x9c: // raw 8-bit ST
			return i + 1
		case s[i] == 0xc2 && i+1 < len(s) && s[i+1] == 0x9c: // UTF-8 encoded ST
			return i + 2
		case s[i] == 0x1b:
			if i+1 < len(s) && s[i+1] == '\\' {
				return i + 2
			}
			// A nested ESC that isn't ST also terminates the string sequence, so
			// stop here and let the outer scan handle it as a new sequence.
			return i
		}
	}
	return len(s) // unterminated: take the rest
}

// allowedInChrome is the whitelist predicate: printable, non-format, non-control.
func allowedInChrome(r rune) bool {
	switch {
	case r == utf8.RuneError:
		return true // handled by the caller as a replacement char
	case r < 0x20, r == 0x7f: // C0 + DEL
		return false
	case r >= 0x80 && r <= 0x9f: // C1
		return false
	case r == 0xad: // soft hyphen: invisible, no business in chrome
		return false
	case unicode.Is(unicode.Cf, r): // bidi overrides, ZWJ, interlinear annotation
		return false
	case unicode.Is(unicode.Cs, r), unicode.Is(unicode.Co, r):
		return false
	case !unicode.IsPrint(r) && r != ' ':
		return false
	}
	return true
}

// ChromeLine is Chrome with runs of whitespace collapsed to single spaces and
// the result trimmed — right for values that must occupy exactly one line, like
// a multi-line Bash command in an approval row.
func ChromeLine(s string, max int) string {
	return Chrome(strings.Join(strings.Fields(s), " "), max)
}
