package bridge

import (
	"os"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A NUL byte on the wire is what put a literal "^@" in the mate's prompt in the
// screenshots. EncodeKey must never produce one: a key it cannot identify sends
// nothing at all.
func TestEncodeKeyNeverEmitsNUL(t *testing.T) {
	cases := []struct {
		name string
		key  tea.KeyMsg
	}{
		{
			// The Windows case. bubbletea's console decoder reports a key-down record
			// for the modifier keys themselves; VK_CONTROL/VK_MENU match nothing in
			// its table, so it falls through to KeyRunes and fills Runes with the
			// record's Char, which for a bare modifier is 0.
			name: "KeyRunes carrying a NUL rune (bare ctrl/alt/win on Windows)",
			key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}},
		},
		{
			name: "the same with alt held",
			key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}, Alt: true},
		},
		{
			// tea.KeyType(0) is keyNUL, KeyCtrlAt, AND the zero value of KeyType, and
			// nothing on tea.KeyMsg distinguishes them.
			name: "KeyType(0)",
			key:  tea.KeyMsg{Type: 0},
		},
		{
			name: "the zero KeyMsg",
			key:  tea.KeyMsg{},
		},
		{
			name: "KeyCtrlAt, which is the same value",
			key:  tea.KeyMsg{Type: tea.KeyCtrlAt},
		},
		{
			name: "a paste of nothing but NUL",
			key:  tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0, 0}, Paste: true},
		},
		{
			name: "a key type with no terminal representation at all",
			key:  tea.KeyMsg{Type: tea.KeyType(-9999)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, appCursor := range []bool{false, true} {
				for _, bracketed := range []bool{false, true} {
					got := EncodeKey(tc.key, appCursor, bracketed)
					if len(got) != 0 {
						t.Errorf("appCursor=%v bracketed=%v: sent %q, want nothing",
							appCursor, bracketed, got)
					}
				}
			}
		})
	}
}

// A NUL mixed into real runes is dropped without taking the real runes with it.
func TestEncodeKeyStripsNULFromMixedRunes(t *testing.T) {
	got := EncodeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a', 0, 'b'}}, false, false)
	if string(got) != "ab" {
		t.Errorf("got %q, want %q", got, "ab")
	}
	got = EncodeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0, 'x', 0}, Paste: true}, false, true)
	if string(got) != "\x1b[200~x\x1b[201~" {
		t.Errorf("paste got %q", got)
	}
}

// The C0 branch still has to encode every real control key, or narrowing it to 1..31
// would have broken ^A through ^Z.
func TestEncodeKeyStillCoversTheWholeC0Range(t *testing.T) {
	for b := 1; b <= 31; b++ {
		got := EncodeKey(tea.KeyMsg{Type: tea.KeyType(b)}, false, false)
		if len(got) != 1 || got[0] != byte(b) {
			t.Errorf("KeyType(%d) encoded to %q, want a single 0x%02x", b, got, b)
		}
	}
	// The ones the bridge and Claude Code both care about, by name.
	for _, tc := range []struct {
		k    tea.KeyType
		want byte
	}{
		{tea.KeyCtrlA, 0x01},
		{tea.KeyCtrlB, 0x02}, // the old prefix
		{tea.KeyCtrlC, 0x03},
		{tea.KeyTab, 0x09},
		{tea.KeyEnter, 0x0d},
		{tea.KeyEsc, 0x1b},
	} {
		got := EncodeKey(tea.KeyMsg{Type: tc.k}, false, false)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%v encoded to %q, want 0x%02x", tc.k, got, tc.want)
		}
	}
	if got := EncodeKey(tea.KeyMsg{Type: tea.KeyBackspace}, false, false); string(got) != "\x7f" {
		t.Errorf("backspace encoded to %q, want DEL", got)
	}
}

func TestEncodeKeyAltPrefixesWithEscape(t *testing.T) {
	got := EncodeKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}, Alt: true}, false, false)
	if string(got) != "\x1b2" {
		t.Errorf("alt+2 encoded to %q, want ESC 2", got)
	}
	got = EncodeKey(tea.KeyMsg{Type: tea.KeyEsc, Alt: true}, false, false)
	if string(got) != "\x1b\x1b" {
		t.Errorf("alt+esc encoded to %q, want ESC ESC", got)
	}
}

// altDigit is what makes alt+1-9 work while typing. The two shapes below are what
// the unix ANSI parser and the Windows console decoder each produce.
func TestAltDigitRecognisesBothReportedShapes(t *testing.T) {
	for _, tc := range []struct {
		name string
		key  tea.KeyMsg
		want int
		ok   bool
	}{
		{"runes shape", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}, Alt: true}, 3, true},
		{"alt+9", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'9'}, Alt: true}, 9, true},
		{"no alt", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'3'}}, 0, false},
		{"alt+0 is not a mate", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'0'}, Alt: true}, 0, false},
		{"alt+letter", tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'a'}, Alt: true}, 0, false},
		{"alt+esc", tea.KeyMsg{Type: tea.KeyEsc, Alt: true}, 0, false},
		{
			// A paste whose text starts with a digit must never be read as a mate
			// switch.
			"a pasted digit",
			tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}, Alt: true, Paste: true}, 0, false,
		},
	} {
		got, ok := altDigit(tc.key)
		if ok != tc.ok || got != tc.want {
			t.Errorf("%s: altDigit = (%d, %v), want (%d, %v)", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// The measurement instrumentation. It is env-gated, so the default must be that no
// file is opened and nothing is recorded.
func TestKeyLogIsOffUnlessTheEnvVarIsSet(t *testing.T) {
	t.Setenv(keyLogEnv, "")
	if l := openKeyLog(); l != nil {
		t.Fatal("keylog opened with the env var empty")
	}
	// A nil log must be safe to use, or every call site would need a branch.
	var nilLog *keyLog
	nilLog.record(tea.KeyMsg{Type: tea.KeyEnter}, "TYPE", []byte{'\r'}, true)
	nilLog.Close()
	if nilLog.count() != 0 {
		t.Error("a nil keylog recorded something")
	}
}

func TestKeyLogRecordsEveryFieldTheMeasurementNeeds(t *testing.T) {
	path := t.TempDir() + "/keys.log"
	t.Setenv(keyLogEnv, path)
	l := openKeyLog()
	if l == nil {
		t.Fatal("keylog did not open")
	}

	// The suspect event: a bare modifier key on Windows.
	l.record(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{0}}, "TYPE", nil, false)
	// A key that does produce bytes.
	l.record(tea.KeyMsg{Type: tea.KeyCtrlB}, "TYPE", []byte{0x02}, true)
	// A paste.
	l.record(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("hi"), Paste: true},
		"TYPE", []byte("\x1b[200~hi\x1b[201~"), true)
	l.Close()

	body := readFile(t, path)
	lines := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(lines) != 4 { // header + 3 events
		t.Fatalf("keylog has %d lines, want 4:\n%s", len(lines), body)
	}
	if !strings.Contains(lines[0], "goos=") {
		t.Errorf("no header line: %q", lines[0])
	}

	// Line 1: every field the measurement needs, and encoded="" so a dropped key is
	// visibly dropped rather than absent.
	for _, want := range []string{
		"type=-1", `string="\x00"`, `typename="runes"`, `runes="\x00"`,
		"codepoints=[0]", "alt=false", "paste=false", "mode=TYPE",
		`encoded=""`, "nbytes=0", "sent=false",
	} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("bare-modifier line is missing %s:\n%s", want, lines[1])
		}
	}
	// Line 2: bytes are hex, so a NUL would show as "00" instead of vanishing.
	for _, want := range []string{"type=2", `string="ctrl+b"`, `encoded="02"`, "sent=true"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("ctrl+b line is missing %s:\n%s", want, lines[2])
		}
	}
	if !strings.Contains(lines[3], "paste=true") || !strings.Contains(lines[3], "1b 5b 32 30 30 7e") {
		t.Errorf("paste line is wrong:\n%s", lines[3])
	}
}

// The keylog has to be wired into the real key path, not just callable.
func TestKeyLogCapturesKeysThroughTheModel(t *testing.T) {
	path := t.TempDir() + "/model-keys.log"
	t.Setenv(keyLogEnv, path)

	f := newFakeShip(t)
	f.spawnPTY("rigger", "")
	f.setRoster(MateStatus{Persona: "rigger", Status: "idle"})
	h := setup(t, f)
	h.poll()
	if h.m.keylog == nil {
		t.Fatal("the model did not open a keylog with the env var set")
	}
	h.key("2") // navigate: nothing sent
	h.settle()
	h.typeAt()
	h.key("ctrl+b") // type: passes through
	h.settle()
	h.m.keylog.Close()

	body := readFile(t, path)
	if !strings.Contains(body, "mode=NAVIGATE") {
		t.Errorf("no Navigate-mode key recorded:\n%s", body)
	}
	if !strings.Contains(body, `mode=TYPE encoded="02" nbytes=1 sent=true`) {
		t.Errorf("ctrl+b was not recorded as sent in Type mode:\n%s", body)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
