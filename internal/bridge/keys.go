package bridge

import (
	"unicode/utf8"

	tea "github.com/charmbracelet/bubbletea"
)

// EncodeKey turns a Bubble Tea key event back into the bytes a real terminal
// would have put on the wire, so they can be POSTed to /pty/{persona}/input.
//
// Bubble Tea owns raw mode and input parsing on all three platforms (that is the
// main reason to use it — no termios, no ConPTY input decoding here), which
// leaves exactly one job: re-encode. Two pieces of terminal state change the
// encoding, and both are read from the persona's own latched DEC private modes:
//
//	appCursor (DECCKM, mode 1)  — arrows and Home/End use SS3 (ESC O A) instead
//	                              of CSI (ESC [ A)
//	bracketedPaste (mode 2004)  — a paste is wrapped in ESC [200~ … ESC [201~
//
// A nil return means "nothing to send" (a key with no terminal representation).
func EncodeKey(k tea.KeyMsg, appCursor, bracketedPaste bool) []byte {
	// A paste arrives as one KeyRunes event with Paste set. Wrapping it in the
	// bracketed-paste markers is what stops a pasted newline from submitting
	// prematurely, and what stops a pasted control sequence from being read as
	// keystrokes by the receiving TUI.
	if k.Paste {
		body := runeBytes(k.Runes)
		if len(body) == 0 {
			// An empty paste must not send bare bracketed-paste markers: the mate
			// would see a paste begin and end with nothing between it.
			return nil
		}
		if !bracketedPaste {
			return body
		}
		out := make([]byte, 0, len(body)+12)
		out = append(out, "\x1b[200~"...)
		out = append(out, body...)
		out = append(out, "\x1b[201~"...)
		return out
	}

	var body []byte
	switch {
	case k.Type == tea.KeyRunes:
		body = runeBytes(k.Runes)
	case k.Type == tea.KeySpace:
		body = []byte{' '}
	case k.Type >= 1 && k.Type <= 31:
		// The C0 range: bubbletea's KeyType values for control keys ARE the
		// byte values (KeyCtrlA == 1, KeyEnter == 13, KeyEsc == 27, …), so this
		// one branch covers every ctrl+letter plus enter, tab and escape.
		//
		// It deliberately starts at 1. See the KeyType(0) case below.
		body = []byte{byte(k.Type)}
	case k.Type == tea.KeyBackspace:
		body = []byte{0x7f}
	case k.Type == 0:
		// tea.KeyType(0) is overloaded three ways and tea.KeyMsg carries no field
		// that tells them apart: it is bubbletea's keyNUL, it is the exported
		// alias KeyCtrlAt, and it is the zero value of KeyType — so any
		// incompletely-populated KeyMsg looks exactly like a real ctrl+@.
		//
		// The old code let all three fall into the C0 branch and POSTed a 0x00,
		// which Claude Code renders as a literal "^@" in its prompt and otherwise
		// ignores. A key the bridge cannot identify must send nothing at all
		// rather than a byte the mate will paint on screen, so KeyType(0) is
		// dropped. The cost is that ctrl+@ / ctrl+space cannot be forwarded;
		// neither is a Claude Code binding, and no agent TUI in this fleet reads
		// NUL.
		return nil
	default:
		if seq, ok := keySequences[k.Type]; ok {
			if appCursor {
				if alt, ok := ss3Sequences[k.Type]; ok {
					body = []byte(alt)
					break
				}
			}
			body = []byte(seq)
		}
	}
	if len(body) == 0 {
		return nil
	}
	// alt+<key> is the key's own bytes with an ESC prefix.
	if k.Alt {
		out := make([]byte, 0, len(body)+1)
		out = append(out, 0x1b)
		return append(out, body...)
	}
	return body
}

// runeBytes UTF-8 encodes the runes of a KeyRunes event, dropping NUL.
//
// A NUL rune in Runes is never something an operator typed. On Windows,
// bubbletea's console decoder (key_windows.go readConInputs/keyType) forwards a
// key-down record for the modifier keys themselves — VK_CONTROL, VK_MENU,
// VK_CAPITAL, the Windows key — and for those the virtual key code matches
// nothing in its table, so keyType falls through to KeyRunes and readConInputs
// then sets Runes to the record's Char, which for a bare modifier is 0. So
// merely holding Ctrl down used to POST a 0x00 to the mate and print "^@" in its
// prompt. Filtering here rather than in the model keeps the rule in the one place
// that owns the wire format.
func runeBytes(runes []rune) []byte {
	out := make([]byte, 0, len(runes)*utf8.UTFMax)
	for _, r := range runes {
		if r == 0 {
			continue
		}
		out = utf8.AppendRune(out, r)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// keySequences is the CSI/SS3 form of every non-C0 key bubbletea reports, using
// xterm's default encodings — the same ones xterm.js sends, so a mate cannot
// tell a bridge pane from the web client's terminal.
var keySequences = map[tea.KeyType]string{
	tea.KeyUp:    "\x1b[A",
	tea.KeyDown:  "\x1b[B",
	tea.KeyRight: "\x1b[C",
	tea.KeyLeft:  "\x1b[D",

	tea.KeyShiftTab: "\x1b[Z",
	tea.KeyHome:     "\x1b[H",
	tea.KeyEnd:      "\x1b[F",
	tea.KeyPgUp:     "\x1b[5~",
	tea.KeyPgDown:   "\x1b[6~",
	tea.KeyDelete:   "\x1b[3~",
	tea.KeyInsert:   "\x1b[2~",

	tea.KeyCtrlPgUp:   "\x1b[5;5~",
	tea.KeyCtrlPgDown: "\x1b[6;5~",

	tea.KeyCtrlUp:    "\x1b[1;5A",
	tea.KeyCtrlDown:  "\x1b[1;5B",
	tea.KeyCtrlRight: "\x1b[1;5C",
	tea.KeyCtrlLeft:  "\x1b[1;5D",
	tea.KeyCtrlHome:  "\x1b[1;5H",
	tea.KeyCtrlEnd:   "\x1b[1;5F",

	tea.KeyShiftUp:    "\x1b[1;2A",
	tea.KeyShiftDown:  "\x1b[1;2B",
	tea.KeyShiftRight: "\x1b[1;2C",
	tea.KeyShiftLeft:  "\x1b[1;2D",
	tea.KeyShiftHome:  "\x1b[1;2H",
	tea.KeyShiftEnd:   "\x1b[1;2F",

	tea.KeyCtrlShiftUp:    "\x1b[1;6A",
	tea.KeyCtrlShiftDown:  "\x1b[1;6B",
	tea.KeyCtrlShiftRight: "\x1b[1;6C",
	tea.KeyCtrlShiftLeft:  "\x1b[1;6D",
	tea.KeyCtrlShiftHome:  "\x1b[1;6H",
	tea.KeyCtrlShiftEnd:   "\x1b[1;6F",

	tea.KeyF1:  "\x1bOP",
	tea.KeyF2:  "\x1bOQ",
	tea.KeyF3:  "\x1bOR",
	tea.KeyF4:  "\x1bOS",
	tea.KeyF5:  "\x1b[15~",
	tea.KeyF6:  "\x1b[17~",
	tea.KeyF7:  "\x1b[18~",
	tea.KeyF8:  "\x1b[19~",
	tea.KeyF9:  "\x1b[20~",
	tea.KeyF10: "\x1b[21~",
	tea.KeyF11: "\x1b[23~",
	tea.KeyF12: "\x1b[24~",
	tea.KeyF13: "\x1b[1;2P",
	tea.KeyF14: "\x1b[1;2Q",
	tea.KeyF15: "\x1b[1;2R",
	tea.KeyF16: "\x1b[1;2S",
	tea.KeyF17: "\x1b[15;2~",
	tea.KeyF18: "\x1b[17;2~",
	tea.KeyF19: "\x1b[18;2~",
	tea.KeyF20: "\x1b[19;2~",
}

// ss3Sequences override keySequences while the mate has DECCKM (application
// cursor keys) latched. Only the cursor cluster changes form.
var ss3Sequences = map[tea.KeyType]string{
	tea.KeyUp:    "\x1bOA",
	tea.KeyDown:  "\x1bOB",
	tea.KeyRight: "\x1bOC",
	tea.KeyLeft:  "\x1bOD",
	tea.KeyHome:  "\x1bOH",
	tea.KeyEnd:   "\x1bOF",
}
