package vt

import (
	"regexp"
	"strings"
	"testing"
)

func write(t *testing.T, s *Screen, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if _, err := s.WriteString(p); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
}

func wantRow(t *testing.T, s *Screen, y int, want string) {
	t.Helper()
	got := strings.TrimRight(s.RowText(y), " ")
	if got != want {
		t.Errorf("row %d = %q, want %q", y, got, want)
	}
}

func TestPlainTextAndCR(t *testing.T) {
	s := New(10, 3)
	write(t, s, "hello", "\r\n", "world")
	wantRow(t, s, 0, "hello")
	wantRow(t, s, 1, "world")
	x, y, _ := s.Cursor()
	if x != 5 || y != 1 {
		t.Errorf("cursor = (%d,%d), want (5,1)", x, y)
	}
}

func TestAutoWrap(t *testing.T) {
	s := New(5, 3)
	write(t, s, "abcdefgh")
	wantRow(t, s, 0, "abcde")
	wantRow(t, s, 1, "fgh")
}

func TestNoWrapWhenDisabled(t *testing.T) {
	s := New(5, 2)
	write(t, s, "\x1b[?7l", "abcdefgh")
	wantRow(t, s, 0, "abcdh") // last column keeps being overwritten
	wantRow(t, s, 1, "")
}

func TestCursorAddressing(t *testing.T) {
	s := New(10, 4)
	write(t, s, "\x1b[3;4Hxy")
	wantRow(t, s, 2, "   xy")

	write(t, s, "\x1b[H", "A")
	wantRow(t, s, 0, "A")

	// CUU/CUD/CUF/CUB
	write(t, s, "\x1b[2;2H", "\x1b[1A", "\x1b[2C", "Z")
	wantRow(t, s, 0, "A  Z")
}

func TestEraseDisplayAndLine(t *testing.T) {
	s := New(6, 3)
	write(t, s, "aaaaaa\r\nbbbbbb\r\ncccccc")

	write(t, s, "\x1b[2;3H\x1b[K") // EL 0: to end of line
	wantRow(t, s, 1, "bb")

	write(t, s, "\x1b[3;3H\x1b[1K") // EL 1: to start of line inclusive
	wantRow(t, s, 2, "   ccc")

	write(t, s, "\x1b[2J")
	for y := 0; y < 3; y++ {
		wantRow(t, s, y, "")
	}
}

func TestEraseDisplayBelowAndAbove(t *testing.T) {
	s := New(4, 3)
	write(t, s, "aaaa\r\nbbbb\r\ncccc", "\x1b[2;3H", "\x1b[J")
	wantRow(t, s, 0, "aaaa")
	wantRow(t, s, 1, "bb")
	wantRow(t, s, 2, "")

	s2 := New(4, 3)
	write(t, s2, "aaaa\r\nbbbb\r\ncccc", "\x1b[2;3H", "\x1b[1J")
	wantRow(t, s2, 0, "")
	wantRow(t, s2, 1, "   b")
	wantRow(t, s2, 2, "cccc")
}

func TestInsertDeleteChars(t *testing.T) {
	s := New(8, 1)
	write(t, s, "abcdef", "\x1b[1;3H", "\x1b[2@") // ICH 2 at col 3
	wantRow(t, s, 0, "ab  cdef")

	s2 := New(8, 1)
	write(t, s2, "abcdef", "\x1b[1;3H", "\x1b[2P") // DCH 2
	wantRow(t, s2, 0, "abef")

	s3 := New(8, 1)
	write(t, s3, "abcdef", "\x1b[1;3H", "\x1b[3X") // ECH 3
	wantRow(t, s3, 0, "ab   f")
}

func TestInsertDeleteLines(t *testing.T) {
	s := New(4, 4)
	write(t, s, "1111\r\n2222\r\n3333\r\n4444")
	write(t, s, "\x1b[2;1H", "\x1b[1L") // IL at row 2
	wantRow(t, s, 0, "1111")
	wantRow(t, s, 1, "")
	wantRow(t, s, 2, "2222")
	wantRow(t, s, 3, "3333")

	s2 := New(4, 4)
	write(t, s2, "1111\r\n2222\r\n3333\r\n4444")
	write(t, s2, "\x1b[2;1H", "\x1b[1M") // DL at row 2
	wantRow(t, s2, 0, "1111")
	wantRow(t, s2, 1, "3333")
	wantRow(t, s2, 2, "4444")
	wantRow(t, s2, 3, "")
}

func TestScrollOnLineFeedAtBottom(t *testing.T) {
	s := New(4, 3)
	write(t, s, "aa\r\nbb\r\ncc\r\ndd")
	wantRow(t, s, 0, "bb")
	wantRow(t, s, 1, "cc")
	wantRow(t, s, 2, "dd")
}

func TestScrollRegion(t *testing.T) {
	s := New(4, 4)
	write(t, s, "1111\r\n2222\r\n3333\r\n4444")
	// Confine scrolling to rows 2-3, park on row 3, then LF.
	write(t, s, "\x1b[2;3r", "\x1b[3;1H", "\n", "XX")
	wantRow(t, s, 0, "1111") // outside the region, untouched
	wantRow(t, s, 1, "3333")
	wantRow(t, s, 2, "XX")
	wantRow(t, s, 3, "4444") // outside the region, untouched
}

func TestReverseIndexScrollsDown(t *testing.T) {
	s := New(4, 3)
	write(t, s, "aa\r\nbb\r\ncc", "\x1b[H", "\x1bM")
	wantRow(t, s, 0, "")
	wantRow(t, s, 1, "aa")
	wantRow(t, s, 2, "bb")
}

func TestSaveRestoreCursor(t *testing.T) {
	s := New(10, 3)
	write(t, s, "\x1b[2;5H", "\x1b7", "\x1b[H", "X", "\x1b8", "Y")
	wantRow(t, s, 0, "X")
	wantRow(t, s, 1, "    Y")
}

func TestAltScreenPreservesPrimary(t *testing.T) {
	s := New(6, 2)
	write(t, s, "primary")
	if s.AltScreen() {
		t.Fatal("started on alt screen")
	}
	write(t, s, "\x1b[?1049h")
	if !s.AltScreen() {
		t.Fatal("1049h did not switch to alt screen")
	}
	wantRow(t, s, 0, "") // alt buffer starts clean
	write(t, s, "alt")
	wantRow(t, s, 0, "alt")

	write(t, s, "\x1b[?1049l")
	if s.AltScreen() {
		t.Fatal("1049l did not leave alt screen")
	}
	wantRow(t, s, 0, "primar")
	wantRow(t, s, 1, "y")
}

func TestModeLatching(t *testing.T) {
	s := New(10, 2)
	write(t, s, "\x1b[?2004h\x1b[?1h\x1b[?25l\x1b[?1000;1002h")
	for _, n := range []int{2004, 1, 1000, 1002} {
		if !s.Mode(n) {
			t.Errorf("mode %d not latched on", n)
		}
	}
	if s.Mode(25) {
		t.Error("mode 25 should be off")
	}
	if _, _, vis := s.Cursor(); vis {
		t.Error("cursor should be hidden after ?25l")
	}
	write(t, s, "\x1b[?2004l")
	if s.Mode(2004) {
		t.Error("mode 2004 should be off after ?2004l")
	}
}

func TestSGRAttributesReachRender(t *testing.T) {
	s := New(20, 1)
	write(t, s, "\x1b[1;31mred\x1b[0m plain")
	line := s.Lines(RenderOpts{})[0]
	if !strings.Contains(line, "\x1b[0;1;31m") {
		t.Errorf("expected bold+red SGR in %q", line)
	}
	if !strings.Contains(line, "red") || !strings.Contains(line, "plain") {
		t.Errorf("text missing from %q", line)
	}
}

func TestSGR256AndTruecolor(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"\x1b[38;5;208mX", "\x1b[0;38;5;208m"},
		{"\x1b[48;2;1;2;3mX", "\x1b[0;48;2;1;2;3m"},
		{"\x1b[38:5:208mX", "\x1b[0;38;5;208m"},
		{"\x1b[38:2::1:2:3mX", "\x1b[0;38;2;1;2;3m"},
		{"\x1b[38:2:1:2:3mX", "\x1b[0;38;2;1;2;3m"},
		{"\x1b[90mX", "\x1b[0;90m"},
		{"\x1b[100mX", "\x1b[0;100m"},
	}
	for _, c := range cases {
		s := New(4, 1)
		write(t, s, c.in)
		got := s.Lines(RenderOpts{})[0]
		if !strings.Contains(got, c.want) {
			t.Errorf("input %q rendered %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

func TestUTF8SplitAcrossWrites(t *testing.T) {
	s := New(10, 1)
	full := []byte("héllo→")
	// Feed one byte at a time; a multi-byte rune is therefore always split.
	for _, b := range full {
		if _, err := s.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	wantRow(t, s, 0, "héllo→")
}

func TestWideRuneOccupiesTwoCells(t *testing.T) {
	s := New(6, 1)
	write(t, s, "日本")
	wantRow(t, s, 0, "日本")
	if x, _, _ := s.Cursor(); x != 4 {
		t.Errorf("cursor x = %d, want 4 after two wide runes", x)
	}
}

func TestDSRAndDAReplies(t *testing.T) {
	s := New(20, 5)
	write(t, s, "\x1b[3;7H", "\x1b[6n")
	if got := string(s.TakeReplies()); got != "\x1b[3;7R" {
		t.Errorf("CPR = %q, want %q", got, "\x1b[3;7R")
	}
	if s.TakeReplies() != nil {
		t.Error("replies should be drained by TakeReplies")
	}
	write(t, s, "\x1b[5n")
	if got := string(s.TakeReplies()); got != "\x1b[0n" {
		t.Errorf("DSR-5 = %q", got)
	}
	write(t, s, "\x1b[c")
	if got := string(s.TakeReplies()); got != "\x1b[?1;2c" {
		t.Errorf("DA1 = %q", got)
	}
}

func TestBellCounted(t *testing.T) {
	s := New(4, 1)
	write(t, s, "a\ab\a\a")
	if n := s.TakeBells(); n != 3 {
		t.Errorf("bells = %d, want 3", n)
	}
	if n := s.TakeBells(); n != 0 {
		t.Errorf("bells not cleared, got %d", n)
	}
	wantRow(t, s, 0, "ab")
}

func TestOSCTitleCapturedClipboardIgnored(t *testing.T) {
	s := New(20, 1)
	write(t, s, "\x1b]0;my title\x07")
	if s.Title() != "my title" {
		t.Errorf("title = %q", s.Title())
	}
	// OSC 52 is the clipboard write. It must be consumed and dropped, and it
	// must not disturb the following text.
	write(t, s, "\x1b]52;c;aGVsbG8=\x1b\\", "after")
	wantRow(t, s, 0, "after")
	if strings.Contains(s.Title(), "hello") || strings.Contains(s.Title(), "52") {
		t.Errorf("OSC 52 leaked into title: %q", s.Title())
	}
}

func TestOSCTitleSanitizedAndBounded(t *testing.T) {
	s := New(10, 1)
	write(t, s, "\x1b]2;"+strings.Repeat("t", 500)+"\x07")
	if len(s.Title()) > 120 {
		t.Errorf("title not bounded: %d runes", len(s.Title()))
	}
}

func TestDCSPayloadDiscarded(t *testing.T) {
	s := New(20, 1)
	write(t, s, "\x1bP+q544e\x1b\\", "visible")
	wantRow(t, s, 0, "visible")
}

func TestC1ViaUTF8IsNotACSI(t *testing.T) {
	s := New(20, 1)
	// U+009B is the 8-bit CSI. Arriving as UTF-8 (0xC2 0x9B) it must be dropped,
	// not treated as an introducer — otherwise a filter that only looks for ESC
	// can be bypassed.
	write(t, s, "a2Jb")
	wantRow(t, s, 0, "a2Jb")
}

func TestRawC1ByteIsNotAnIntroducer(t *testing.T) {
	// The same smuggling attempt as a bare byte, which is invalid UTF-8. The
	// decoder must yield U+FFFD and leave "2J" as text rather than clearing the
	// screen. Written as an explicit byte slice so the intent survives editing.
	s := New(20, 2)
	write(t, s, "keep\r\n")
	if _, err := s.Write([]byte{'a', 0x9b, '2', 'J', 'b'}); err != nil {
		t.Fatal(err)
	}
	wantRow(t, s, 0, "keep")
	got := strings.TrimRight(s.RowText(1), " ")
	if !strings.HasPrefix(got, "a") || !strings.HasSuffix(got, "2Jb") {
		t.Errorf("raw 0x9b acted as an introducer: %q", got)
	}
	// Same for the 8-bit OSC (0x9d), which would otherwise swallow the rest of
	// the line looking for a terminator.
	s2 := New(20, 1)
	if _, err := s2.Write([]byte{0x9d, '0', ';', 'x', 0x07, 'v', 'i', 's'}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s2.RowText(0), "vis") {
		t.Errorf("raw 0x9d swallowed the line: %q", s2.RowText(0))
	}
	if s2.Title() != "" {
		t.Errorf("raw 0x9d set a title: %q", s2.Title())
	}
}

func TestUnknownSequencesAreConsumed(t *testing.T) {
	s := New(20, 2)
	write(t, s,
		"\x1b[>4;2m",      // xterm modifyOtherKeys — not implemented
		"\x1b[?2026h",     // synchronized output — latched, no visual effect
		"\x1b#8",          // DECALN — consumed
		"\x1b(B",          // charset designator
		"\x1b]11;?\x1b\\", // background colour query
		"ok",
	)
	wantRow(t, s, 0, "ok")
}

// hostileEscapes is the security-critical test: whatever a mate writes, the
// rendered lines may only contain SGR sequences that this package generated.
// Anything else (cursor moves, screen clears, OSC, DCS, mode changes) reaching
// the operator's real terminal would let agent output spoof or destroy the UI.
func TestRenderedOutputContainsOnlySGR(t *testing.T) {
	hostile := strings.Join([]string{
		"\x1b[2J\x1b[H",                     // clear + home
		"\x1b]0;PWNED\x07",                  // title
		"\x1b]52;c;cGF5bG9hZA==\x1b\\",      // clipboard write
		"\x1b[?1049h",                       // alt screen
		"\x1b[999;999H",                     // far cursor move
		"\x1b[31mred\x1b[0m",                // legitimate colour
		"\x1bPtmux;\x1b\x1b]0;nested\x07\\", // DCS-wrapped OSC
		"\x1b_APC payload\x1b\\",
		"\x1b[8m\x1b[38;2;9;9;9mtext",
		"literal \x1b escape then text",
		"\x00\x01\x02\x7f",
		"\x1b[6n", // DSR: goes to replies, must not appear in the render
	}, "")

	s := New(40, 6)
	write(t, s, hostile)
	rendered := strings.Join(s.Lines(RenderOpts{ShowCursor: true}), "\n")

	// Every escape in the output must be a complete SGR (ESC [ digits/; m).
	sgr := regexp.MustCompile(`^\x1b\[[0-9;]*m$`)
	for i := 0; i < len(rendered); i++ {
		if rendered[i] != 0x1b {
			continue
		}
		// Find the end of this sequence: the next byte in @..~ after ESC [.
		end := -1
		for j := i + 1; j < len(rendered); j++ {
			c := rendered[j]
			if c >= '@' && c <= '~' && j > i+1 {
				end = j
				break
			}
			if c == 0x1b {
				break
			}
		}
		if end < 0 {
			t.Fatalf("dangling escape at %d in %q", i, rendered)
		}
		seq := rendered[i : end+1]
		if !sgr.MatchString(seq) {
			t.Fatalf("non-SGR escape %q leaked into rendered output", seq)
		}
		i = end
	}
	// Sanity: the render is not empty and did carry the colour through.
	if !strings.Contains(rendered, "\x1b[0;31m") {
		t.Errorf("expected the legitimate red SGR to survive; got %q", rendered)
	}
	// And nothing outside the grid: the far cursor move must have been clamped.
	if x, y, _ := s.Cursor(); x >= 40 || y >= 6 {
		t.Errorf("cursor escaped the grid: (%d,%d)", x, y)
	}
}

func TestBoundsAgainstAbusiveInput(t *testing.T) {
	s := New(20, 4)
	// A huge parameter list, a huge OSC payload, and a huge run of digits must
	// all be absorbed without panicking or resizing the grid.
	write(t, s, "\x1b["+strings.Repeat("1;", 5000)+"m")
	write(t, s, "\x1b]0;"+strings.Repeat("x", 100000)+"\x07")
	write(t, s, "\x1b["+strings.Repeat("9", 10000)+"H")
	write(t, s, "\x1b[999999999999999999999;5H")
	write(t, s, "ok")
	if c, r := s.Size(); c != 20 || r != 4 {
		t.Fatalf("geometry changed to %dx%d", c, r)
	}
	if !strings.Contains(s.String(), "ok") {
		t.Errorf("screen lost its content: %q", s.String())
	}
	// The oversized title was dropped rather than stored.
	if len(s.Title()) > 120 {
		t.Errorf("title escaped its bound: %d", len(s.Title()))
	}
}

func TestReplyQueueBounded(t *testing.T) {
	s := New(10, 2)
	for i := 0; i < 20000; i++ {
		write(t, s, "\x1b[6n")
	}
	if n := len(s.TakeReplies()); n > maxReplies {
		t.Errorf("reply queue grew to %d, cap is %d", n, maxReplies)
	}
}

func TestResizeClampsAndPreservesTopLeft(t *testing.T) {
	s := New(10, 4)
	write(t, s, "abcdefghij\r\nklmno")
	s.Resize(5, 2)
	if c, r := s.Size(); c != 5 || r != 2 {
		t.Fatalf("size = %dx%d", c, r)
	}
	wantRow(t, s, 0, "abcde")
	wantRow(t, s, 1, "klmno")

	s.Resize(-5, 99999)
	c, r := s.Size()
	if c != MinCols || r != MaxRows {
		t.Errorf("clamped size = %dx%d, want %dx%d", c, r, MinCols, MaxRows)
	}
}

func TestResetClearsEverything(t *testing.T) {
	s := New(8, 2)
	write(t, s, "\x1b[?1049h\x1b[31mstuff\x1b]0;t\x07")
	s.Reset()
	if s.AltScreen() {
		t.Error("still on alt screen after reset")
	}
	if s.Title() != "" {
		t.Errorf("title survived reset: %q", s.Title())
	}
	if got := strings.TrimSpace(s.String()); got != "" {
		t.Errorf("content survived reset: %q", got)
	}
	if c, r := s.Size(); c != 8 || r != 2 {
		t.Errorf("reset changed geometry to %dx%d", c, r)
	}
	if s.Mode(1049) {
		t.Error("mode survived reset")
	}
}

func TestCursorRenderedAsReverseVideo(t *testing.T) {
	s := New(5, 1)
	write(t, s, "\x1b[1;1H")
	withCursor := s.Lines(RenderOpts{ShowCursor: true})[0]
	without := s.Lines(RenderOpts{ShowCursor: false})[0]
	if !strings.Contains(withCursor, "\x1b[0;7m") {
		t.Errorf("cursor cell not reverse-video: %q", withCursor)
	}
	if strings.Contains(without, "\x1b[0;7m") {
		t.Errorf("cursor drawn when ShowCursor is false: %q", without)
	}
}

func TestTabStops(t *testing.T) {
	s := New(20, 1)
	write(t, s, "a\tb")
	wantRow(t, s, 0, "a       b")

	s2 := New(20, 1)
	write(t, s2, "\x1b[1;4H\x1bH", "\x1b[1;1H", "x\ty") // custom stop at col 4
	wantRow(t, s2, 0, "x  y")
}

func TestEraseCarriesBackgroundColour(t *testing.T) {
	s := New(4, 1)
	write(t, s, "\x1b[44m\x1b[2K")
	line := s.Lines(RenderOpts{})[0]
	if !strings.Contains(line, "\x1b[0;44m") {
		t.Errorf("erased cells lost the background colour: %q", line)
	}
}

func TestLinesLengthMatchesRows(t *testing.T) {
	s := New(10, 7)
	if got := len(s.Lines(RenderOpts{})); got != 7 {
		t.Errorf("Lines returned %d rows, want 7", got)
	}
}
