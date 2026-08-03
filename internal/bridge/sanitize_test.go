package bridge

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

func TestChromeRemovesWholeEscapeSequences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "cooper", "cooper"},
		{"csi erase display", "a\x1b[2Jb", "ab"},
		{"csi cursor position", "a\x1b[999;999Hb", "ab"},
		{"sgr colour", "a\x1b[31mred\x1b[0mb", "aredb"},
		{"csi with intermediates", "a\x1b[?1049hb", "ab"},
		{"osc title bel terminated", "a\x1b]0;pwned\x07b", "ab"},
		{"osc title st terminated", "a\x1b]0;pwned\x1b\\b", "ab"},
		{"osc clipboard write", "a\x1b]52;c;ZXZpbA==\x07b", "ab"},
		{"dcs", "a\x1bPq#0;2;0;0;0\x1b\\b", "ab"},
		{"apc", "a\x1b_Gf=100\x1b\\b", "ab"},
		{"two byte escape", "a\x1bcb", "ab"},
		{"charset designator", "a\x1b(0b", "ab"},
		{"lone trailing esc", "ab\x1b", "ab"},
		{"unterminated osc eats the tail", "ab\x1b]0;anything after", "ab"},
		{"eight bit csi", "a\u009b2Jb", "ab"},
		{"eight bit osc", "a\u009d0;x\x07b", "ab"},
		{"c0 controls dropped", "a\rb\nc\td\x00e", "abcde"},
		{"del dropped", "a\x7fb", "ab"},
		{"bidi override dropped", "cat \u202esctipt.sh", "cat sctipt.sh"},
		{"rtl mark dropped", "a\u200fb", "ab"},
		{"zero width joiner dropped", "a\u200db", "ab"},
		{"soft hyphen dropped", "a\u00adb", "ab"},
		{"nested esc inside osc", "a\x1b]0;x\x1b[2Jb", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Chrome(tc.in, 0)
			if got != tc.want {
				t.Errorf("Chrome(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertInertChrome(t, got)
		})
	}
}

// assertInertChrome is the property that actually matters: whatever comes out of
// Chrome must be incapable of doing anything to the operator's terminal.
func assertInertChrome(t *testing.T, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Errorf("Chrome produced invalid UTF-8: %q", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			t.Errorf("control byte %#x survived at %d in %q", s[i], i, s)
		}
	}
	for _, r := range s {
		if r >= 0x80 && r <= 0x9f {
			t.Errorf("C1 control %#x survived in %q", r, s)
		}
	}
}

func TestChromeBoundsDisplayWidth(t *testing.T) {
	if got := Chrome(strings.Repeat("x", 100), 10); got != "xxxxxxxxx…" {
		t.Errorf("Chrome truncation = %q", got)
	}
	// Wide runes are counted by display cells, not by count, so a CJK name cannot
	// occupy twice its budget and wrap the tab bar.
	if got := Chrome(strings.Repeat("船", 100), 10); len([]rune(got)) > 6 {
		t.Errorf("wide runes overran the cell budget: %q (%d runes)", got, len([]rune(got)))
	}
	// A pile of zero-width runes must not slip past the bound either: combining
	// marks are printable and kept, so they are counted as one cell each rather
	// than letting 5000 of them ride along inside an 8-cell budget.
	if got := Chrome(strings.Repeat("\u0301", 5000)+"tail", 8); len([]rune(got)) > 9 {
		t.Errorf("zero-width flood overran the bound: %d runes", len([]rune(got)))
	}
}

// TestChromeTruncationFitsTheBudget pins the property the tab bar depends on:
// whatever max is asked for, the result never occupies more than max cells.
func TestChromeTruncationFitsTheBudget(t *testing.T) {
	inputs := []string{
		strings.Repeat("x", 200),
		strings.Repeat("船", 200),
		"short",
		strings.Repeat("e\u0301", 200),
		"mixed 船 x \u0301 text " + strings.Repeat("q", 200),
	}
	for _, in := range inputs {
		for max := 1; max <= 40; max++ {
			got := Chrome(in, max)
			if w := runewidth.StringWidth(got); w > max {
				t.Errorf("Chrome(%.10q…, %d) is %d cells wide: %q", in, max, w, got)
			}
		}
	}
}

func TestChromeLineCollapsesWhitespace(t *testing.T) {
	in := "go test   ./...\n\n  -run\tTestX\r\n"
	if got := ChromeLine(in, 0); got != "go test ./... -run TestX" {
		t.Errorf("ChromeLine = %q", got)
	}
}

func TestChromeReplacesInvalidUTF8(t *testing.T) {
	got := Chrome("ab\xffcd", 0)
	if got != "ab�cd" {
		t.Errorf("Chrome(invalid utf-8) = %q, want %q", got, "ab�cd")
	}
	assertInertChrome(t, got)
}
