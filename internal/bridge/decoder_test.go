package bridge

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// The tests in this file drive Bubble Tea's REAL input decoder.
//
// Every other test in this package builds tea.KeyMsg values directly (see
// keyMsg in harness_test.go), which starts after decoding has already
// happened. That left the layer turning terminal bytes into messages entirely
// unexercised — and the bridge's central promise lives in that layer: "every
// key reaches the mate byte for byte, ^B included".
//
// It matters most for alt+1..9, the bindings that switch mates without leaving
// Type mode. altDigit carries an explicit claim that the digit lands in
// different fields depending on the platform's decoder, and until now nothing
// checked it. If that claim were wrong, alt+2 would silently stop switching
// mates and no test would notice.
//
// # What this covers, and what it does not
//
// Input is an io.Pipe, not a terminal. Bubble Tea reads it with its ANSI
// sequence parser — the same parser a real unix terminal drives — so on Linux
// and macOS this is the production decode path.
//
// On Windows it is not the whole story. Given a real console handle Bubble Tea
// reads console input events rather than parsing bytes, and that branch cannot
// be reached without an actual ConPTY and a child process. What runs here on
// Windows is the ANSI path, which is what a Windows terminal reached over SSH
// produces — the bridge's stated deployment — so it earns its keep there, but
// it does not prove the console-event branch of altDigit. Closing that gap
// needs a real ConPTY and is deliberately out of scope.

// keyProbe is a tea.Model that exists only to capture decoded key messages.
// A channel rather than a slice, so the test goroutine and the Bubble Tea loop
// never share memory — CI runs the race detector.
type keyProbe struct {
	keys chan tea.KeyMsg
}

func (p keyProbe) Init() tea.Cmd { return nil }

func (p keyProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if k, ok := msg.(tea.KeyMsg); ok {
		// ctrl+q ends the program. It is not a bridge binding, so it cannot
		// collide with any sequence under test.
		if k.Type == tea.KeyCtrlQ {
			return p, tea.Quit
		}
		select {
		case p.keys <- k:
		default:
		}
	}
	return p, nil
}

func (p keyProbe) View() string { return "" }

// decode feeds raw terminal bytes through Bubble Tea's decoder and returns the
// key messages produced, in order.
//
// Each sequence is written separately. io.Pipe hands a write straight to the
// reader, so one sequence per write keeps the parser from guessing where one
// ends and the next begins — which matters for a lone ESC, which is only an
// ESC key when nothing follows it.
func decode(t *testing.T, sequences ...string) []tea.KeyMsg {
	t.Helper()

	pr, pw := io.Pipe()
	probe := keyProbe{keys: make(chan tea.KeyMsg, 64)}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	prog := tea.NewProgram(probe,
		tea.WithInput(pr),
		// A decoder test needs no renderer, and a renderer would write to the
		// test's output concurrently for no benefit.
		tea.WithoutRenderer(),
		tea.WithContext(ctx),
	)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = prog.Run()
	}()

	for _, s := range sequences {
		if _, err := pw.Write([]byte(s)); err != nil {
			t.Fatalf("write %q: %v", s, err)
		}
		// Let the parser consume this sequence before the next arrives, so a
		// trailing ESC is not read as the alt prefix of what follows.
		time.Sleep(20 * time.Millisecond)
	}

	got := make([]tea.KeyMsg, 0, len(sequences))
	deadline := time.After(10 * time.Second)
	for len(got) < len(sequences) {
		select {
		case k := <-probe.keys:
			got = append(got, k)
		case <-deadline:
			t.Fatalf("decoder produced %d messages, want %d: %v", len(got), len(sequences), got)
		}
	}

	_, _ = pw.Write([]byte{0x11}) // ctrl+q
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("bubbletea program did not exit after the sentinel key")
	}
	_ = pw.Close()
	_ = pr.Close()
	return got
}

// TestDecoderRecognisesAltDigits is the point of this file. alt+1..9 switch
// mates from inside Type mode; if the decoder reports them in a shape altDigit
// does not recognise, that binding silently dies.
func TestDecoderRecognisesAltDigits(t *testing.T) {
	seqs := make([]string, 0, 9)
	for d := '1'; d <= '9'; d++ {
		seqs = append(seqs, "\x1b"+string(d)) // ESC-prefixed digit is alt+digit
	}
	for i, k := range decode(t, seqs...) {
		want := i + 1
		got, ok := altDigit(k)
		if !ok {
			t.Errorf("altDigit did not recognise %q (String()=%q Type=%v Alt=%v Runes=%q); "+
				"alt+%d would stop switching mates", seqs[i], k.String(), k.Type, k.Alt, string(k.Runes), want)
			continue
		}
		if got != want {
			t.Errorf("altDigit(%q) = %d, want %d", seqs[i], got, want)
		}
	}
}

// A bare digit must NOT read as alt+digit: in Type mode it is text for the
// mate, and treating it as a mate switch would eat the operator's keystroke.
func TestDecoderPlainDigitsAreNotAltDigits(t *testing.T) {
	for i, k := range decode(t, "1", "5", "9") {
		if n, ok := altDigit(k); ok {
			t.Errorf("plain digit %d decoded as altDigit %d — it must reach the mate as text", i, n)
		}
	}
}

// esc leaves Type mode; alt+esc sends a literal ESC to the mate. They must stay
// distinguishable after decoding or one of the two behaviours is unreachable.
func TestDecoderDistinguishesEscFromAltEsc(t *testing.T) {
	got := decode(t, "\x1b", "\x1b\x1b")

	if got[0].Type != tea.KeyEsc || got[0].Alt {
		t.Errorf("bare ESC decoded as Type=%v Alt=%v, want KeyEsc with Alt=false", got[0].Type, got[0].Alt)
	}
	if got[1].Type != tea.KeyEsc || !got[1].Alt {
		t.Errorf("alt+ESC decoded as Type=%v Alt=%v, want KeyEsc with Alt=true", got[1].Type, got[1].Alt)
	}

	// The routing that depends on it: onTypeKey leaves Type mode on a bare esc
	// and sends 0x1b to the mate on alt+esc.
	m := New(Options{Base: "http://127.0.0.1:1"})
	t.Cleanup(m.Close)
	m.mode = modeType
	m.update(got[0])
	if m.mode != modeNavigate {
		t.Error("a decoded bare ESC did not leave Type mode")
	}
}

// ^B is a Claude Code binding, so the bridge must never own it: it has to
// survive decoding and be re-encoded as a literal 0x02 for the mate. This is
// the "there is no prefix key" promise, checked end to end.
func TestCtrlBSurvivesDecodeAndEncodesAsLiteral(t *testing.T) {
	k := decode(t, "\x02")[0]
	if k.Type != tea.KeyCtrlB {
		t.Fatalf("0x02 decoded as Type=%v, want KeyCtrlB", k.Type)
	}
	if got := EncodeKey(k, false, false); !bytes.Equal(got, []byte{0x02}) {
		t.Errorf("EncodeKey(ctrl+b) = %v, want [2] — ^B must reach the mate untouched", got)
	}
}

// A paste must arrive as ONE message with Paste set, not as a burst of
// keystrokes, and must be re-wrapped in the bracketed-paste markers. Otherwise
// a pasted newline submits early and pasted control bytes are read as keys.
func TestDecoderDeliversBracketedPasteAsOneEvent(t *testing.T) {
	k := decode(t, "\x1b[200~hello\nworld\x1b[201~")[0]
	if !k.Paste {
		t.Fatalf("bracketed paste decoded with Paste=false (Type=%v Runes=%q)", k.Type, string(k.Runes))
	}
	if got, want := string(k.Runes), "hello\nworld"; got != want {
		t.Errorf("paste body = %q, want %q", got, want)
	}

	// With the mate in bracketed-paste mode the markers go back on.
	encoded := EncodeKey(k, false, true)
	if !bytes.HasPrefix(encoded, []byte("\x1b[200~")) || !bytes.HasSuffix(encoded, []byte("\x1b[201~")) {
		t.Errorf("EncodeKey(paste, bracketed=true) = %q, want it wrapped in paste markers", encoded)
	}
}

// The whole contract in one assertion: what the operator types is what the mate
// receives. Bytes in through the real decoder, bytes out through EncodeKey.
func TestKeystrokeRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain rune", "i", "i"},
		{"enter", "\r", "\r"},
		{"tab", "\t", "\t"},
		{"ctrl+b", "\x02", "\x02"},
		{"ctrl+c", "\x03", "\x03"},
		{"ctrl+d", "\x04", "\x04"},
		{"backspace", "\x7f", "\x7f"},
		{"up arrow", "\x1b[A", "\x1b[A"},
		{"down arrow", "\x1b[B", "\x1b[B"},
		{"home", "\x1b[H", "\x1b[H"},
	}
	seqs := make([]string, len(cases))
	for i, c := range cases {
		seqs[i] = c.in
	}
	for i, k := range decode(t, seqs...) {
		got := string(EncodeKey(k, false, false))
		if got != cases[i].want {
			t.Errorf("%s: typed %q, mate would receive %q, want %q",
				cases[i].name, cases[i].in, got, cases[i].want)
		}
	}
}

// KeyType(0) is simultaneously KeyNull, KeyCtrlAt, and the zero value of
// KeyType — so any incompletely-populated KeyMsg is indistinguishable from a
// real ctrl+@. Encoding it emitted 0x00, which Claude Code paints as a literal
// "^@" in its prompt.
//
// A key the bridge cannot identify must send nothing at all rather than a byte
// the mate will render.
func TestUnidentifiableKeySendsNothing(t *testing.T) {
	for _, k := range []struct {
		name string
		msg  tea.KeyMsg
	}{
		{"zero-value KeyMsg", tea.KeyMsg{}},
		{"KeyNull", tea.KeyMsg{Type: tea.KeyNull}},
		{"runes with no runes", tea.KeyMsg{Type: tea.KeyRunes}},
	} {
		if got := EncodeKey(k.msg, false, false); len(got) != 0 {
			t.Errorf("EncodeKey(%s) = %v, want no bytes — a byte here paints %q in the mate's prompt",
				k.name, got, "^@")
		}
	}
}
