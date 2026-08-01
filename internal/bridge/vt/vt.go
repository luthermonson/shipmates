// Package vt is a bounded, in-memory terminal emulator: it consumes the raw
// screen bytes of a PTY-hosted mate and maintains a fixed cols×rows cell grid
// that a caller can render as plain lines.
//
// It exists for one security reason and one practical reason.
//
// Security: agent output is untrusted. If a TUI's bytes were handed straight to
// the operator's real terminal, a model or a tool result could home the cursor,
// clear the screen, repaint the tab bar, set the window title, or use OSC 52 to
// write the operator's clipboard. Running those bytes through this emulator
// confines every escape sequence to a virtual screen of known size. The caller
// re-emits only attributes this package computed from a whitelist, so nothing
// the mate writes can reach the real terminal verbatim.
//
// Practical: Bubble Tea composes a frame and diff-repaints it. There is no way
// to interleave raw PTY output with that renderer, so the pane content has to be
// a string. This package produces it.
//
// Coverage is a deliberate subset — the sequences an interactive line-editor or
// full-screen TUI actually emits (see Screen.Write). Anything unrecognised is
// consumed and dropped rather than leaking through.
package vt

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

// Geometry bounds. A remote peer influences the size only indirectly (our own
// window size drives Resize), but the clamps keep a bad value from allocating
// an enormous grid.
const (
	MinCols = 2
	MinRows = 1
	MaxCols = 1000
	MaxRows = 1000
)

// maxParams bounds a CSI parameter list. Real sequences use a handful; the cap
// stops "\x1b[1;1;1;...;1m" with a million parameters from growing a slice
// without limit.
const maxParams = 32

// maxParamValue clamps each parameter. Cursor moves and scroll counts are
// clamped again against the grid, but bounding here keeps arithmetic sane.
const maxParamValue = 65535

// maxStringPayload bounds an OSC/DCS/APC payload. Past this the payload is
// dropped (still consumed, so the terminator resynchronises the parser).
const maxStringPayload = 4096

// maxReplies bounds the queue of device-report answers awaiting delivery to the
// mate, so a flood of DSR queries can't grow memory while we're not writing.
const maxReplies = 4096

// ColorKind distinguishes "inherit the terminal default" from the two ways a
// concrete color can be specified.
type ColorKind uint8

const (
	ColorDefault ColorKind = iota
	ColorIndexed
	ColorRGB
)

// Color is a foreground or background color. Only these three forms are
// representable, which is what makes re-emission safe: there is no path for an
// arbitrary byte string to reach the output.
type Color struct {
	Kind ColorKind
	Idx  uint8
	R    uint8
	G    uint8
	B    uint8
}

// Attr is the whitelist of cell attributes this emulator understands. Anything
// else in an SGR sequence is dropped.
type Attr struct {
	FG        Color
	BG        Color
	Bold      bool
	Faint     bool
	Italic    bool
	Underline bool
	Blink     bool
	Reverse   bool
	Hidden    bool
	Strike    bool
}

// SGR renders the attribute as a full, self-contained SGR sequence beginning
// with a reset. Emitting the complete state (rather than a delta) means a
// renderer can never desynchronise, and every byte produced here comes from
// this function — never from the input stream.
func (a Attr) SGR() string {
	var b strings.Builder
	b.WriteString("\x1b[0")
	if a.Bold {
		b.WriteString(";1")
	}
	if a.Faint {
		b.WriteString(";2")
	}
	if a.Italic {
		b.WriteString(";3")
	}
	if a.Underline {
		b.WriteString(";4")
	}
	if a.Blink {
		b.WriteString(";5")
	}
	if a.Reverse {
		b.WriteString(";7")
	}
	if a.Hidden {
		b.WriteString(";8")
	}
	if a.Strike {
		b.WriteString(";9")
	}
	writeColor(&b, a.FG, 38, 30, 90)
	writeColor(&b, a.BG, 48, 40, 100)
	b.WriteString("m")
	return b.String()
}

func writeColor(b *strings.Builder, c Color, ext, base, bright int) {
	switch c.Kind {
	case ColorIndexed:
		switch {
		case c.Idx < 8:
			b.WriteString(";" + strconv.Itoa(base+int(c.Idx)))
		case c.Idx < 16:
			b.WriteString(";" + strconv.Itoa(bright+int(c.Idx-8)))
		default:
			b.WriteString(";" + strconv.Itoa(ext) + ";5;" + strconv.Itoa(int(c.Idx)))
		}
	case ColorRGB:
		b.WriteString(";" + strconv.Itoa(ext) + ";2;" +
			strconv.Itoa(int(c.R)) + ";" + strconv.Itoa(int(c.G)) + ";" + strconv.Itoa(int(c.B)))
	}
}

// IsZero reports whether the attribute is the terminal default (no SGR needed).
func (a Attr) IsZero() bool { return a == Attr{} }

// Cell is one grid position. Rune 0 means "blank". Cont marks the right half of
// a double-width rune: it holds no rune of its own and is skipped when
// rendering.
type Cell struct {
	Rune rune
	Attr Attr
	Cont bool
}

func (c Cell) blank() bool { return (c.Rune == 0 || c.Rune == ' ') && !c.Cont }

type parseState uint8

const (
	stGround parseState = iota
	stEsc
	stEscInterm // ESC + intermediate (charset designators, line attrs)
	stCSI
	stOSC
	stString // DCS / SOS / PM / APC payload — consumed, never rendered
)

// Screen is a virtual terminal. It is not safe for concurrent use; the bridge
// model owns one per persona and is the single writer.
type Screen struct {
	cols, rows int

	grid  [][]Cell
	alt   [][]Cell
	onAlt bool

	x, y int

	// saved cursor, one per screen buffer (DECSC/DECRC, and the implicit save
	// that mode 1049 performs on entry).
	savePrimary savedCursor
	saveAlt     savedCursor

	attr Attr

	top, bot int // scroll region, 0-based inclusive

	autoWrap      bool
	wrapPending   bool
	cursorVisible bool
	insertMode    bool // IRM (mode 4)
	originMode    bool // DECOM (mode 6)

	// modes latches every DEC private mode seen, so callers can ask whether
	// bracketed paste (2004) or application cursor keys (1) are active and
	// encode their keystrokes to match.
	modes map[int]bool

	tabs map[int]bool // explicit tab stops (HTS/TBC); empty means every 8th col

	// parser
	state    parseState
	params   []byte
	interm   []byte
	strBuf   []byte
	strTrunc bool
	escPrev  byte // for detecting ESC \ (ST) inside string states
	utf8Buf  []byte

	// out-of-band results the caller drains
	replies []byte
	bells   int
	title   string
}

type savedCursor struct {
	x, y   int
	attr   Attr
	origin bool
}

// New allocates a screen. Dimensions are clamped to the package bounds.
func New(cols, rows int) *Screen {
	cols, rows = clampSize(cols, rows)
	s := &Screen{
		cols:          cols,
		rows:          rows,
		autoWrap:      true,
		cursorVisible: true,
		modes:         map[int]bool{},
		tabs:          map[int]bool{},
	}
	s.grid = newGrid(cols, rows)
	s.alt = newGrid(cols, rows)
	s.top, s.bot = 0, rows-1
	return s
}

func clampSize(cols, rows int) (int, int) {
	if cols < MinCols {
		cols = MinCols
	}
	if cols > MaxCols {
		cols = MaxCols
	}
	if rows < MinRows {
		rows = MinRows
	}
	if rows > MaxRows {
		rows = MaxRows
	}
	return cols, rows
}

func newGrid(cols, rows int) [][]Cell {
	g := make([][]Cell, rows)
	for i := range g {
		g[i] = make([]Cell, cols)
	}
	return g
}

// Size returns the current geometry.
func (s *Screen) Size() (cols, rows int) { return s.cols, s.rows }

// Cursor returns the cursor position and whether it should be drawn.
func (s *Screen) Cursor() (x, y int, visible bool) { return s.x, s.y, s.cursorVisible }

// Mode reports the latched state of a DEC private mode (CSI ? Pm h/l).
func (s *Screen) Mode(n int) bool { return s.modes[n] }

// AltScreen reports whether the mate is on the alternate screen buffer.
func (s *Screen) AltScreen() bool { return s.onAlt }

// Title returns the last OSC 0/2 window title, sanitized to printable runes and
// bounded. It is informational only: the bridge shows it in chrome and never
// forwards it to the real terminal.
func (s *Screen) Title() string { return s.title }

// TakeBells returns and clears the count of BEL bytes seen since the last call.
func (s *Screen) TakeBells() int {
	n := s.bells
	s.bells = 0
	return n
}

// TakeReplies returns and clears the device-report bytes the mate is waiting
// for (cursor position report, device attributes). The caller decides whether
// to deliver them — it must only do so when it holds the PTY writer lock, since
// they are writes into a shared terminal.
func (s *Screen) TakeReplies() []byte {
	if len(s.replies) == 0 {
		return nil
	}
	out := s.replies
	s.replies = nil
	return out
}

// Reset returns the screen to power-on state, keeping its geometry. Used when a
// fresh snapshot arrives so a re-prime cannot stack on stale content.
func (s *Screen) Reset() {
	cols, rows := s.cols, s.rows
	fresh := New(cols, rows)
	*s = *fresh
}

// Resize changes the geometry. Content is anchored top-left and clipped; the
// bridge posts a matching PTY resize immediately afterwards, so the mate
// repaints and any clipping is transient.
func (s *Screen) Resize(cols, rows int) {
	cols, rows = clampSize(cols, rows)
	if cols == s.cols && rows == s.rows {
		return
	}
	s.grid = resizeGrid(s.grid, cols, rows)
	s.alt = resizeGrid(s.alt, cols, rows)
	s.cols, s.rows = cols, rows
	s.top, s.bot = 0, rows-1
	s.clampCursor()
	s.wrapPending = false
}

func resizeGrid(g [][]Cell, cols, rows int) [][]Cell {
	out := newGrid(cols, rows)
	for y := 0; y < rows && y < len(g); y++ {
		copy(out[y], g[y][:min(cols, len(g[y]))])
	}
	return out
}

func (s *Screen) buf() [][]Cell {
	if s.onAlt {
		return s.alt
	}
	return s.grid
}

func (s *Screen) clampCursor() {
	s.x = clamp(s.x, 0, s.cols-1)
	s.y = clamp(s.y, 0, s.rows-1)
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// Write feeds screen bytes into the emulator. It never returns an error and
// never blocks: unrecognised or malformed input is dropped.
//
// Recognised:
//
//	C0:   BEL BS HT LF VT FF CR ESC (SO/SI consumed)
//	ESC:  7 8 c D E M H (DECSC DECRC RIS IND NEL RI HTS), = > , charset
//	      designators, ESC # n, ESC ] (OSC), ESC P/X/^/_ (string, discarded)
//	CSI:  @ A B C D E F G H I J K L M P S T X Z ` d f g h l m n r c
//	      including DEC private ?…h/l for 1, 4, 6, 7, 25, 47, 1000-1006,
//	      1007, 1047, 1048, 1049, 2004 (all modes are latched regardless)
//	SGR:  0-9, 21-29, 30-37, 39, 40-47, 49, 90-97, 100-107, 38/48 with
//	      indexed (5;n) and truecolor (2;r;g;b) forms, semicolon or colon
//	      separated
func (s *Screen) Write(p []byte) (int, error) {
	for _, b := range p {
		s.feed(b)
	}
	return len(p), nil
}

// WriteString is Write for a string.
func (s *Screen) WriteString(str string) (int, error) { return s.Write([]byte(str)) }

func (s *Screen) feed(b byte) {
	switch s.state {
	case stGround:
		s.ground(b)
	case stEsc:
		s.escape(b)
	case stEscInterm:
		// One byte of charset / line-attribute selector, then back to ground.
		s.state = stGround
	case stCSI:
		s.csi(b)
	case stOSC:
		s.stringByte(b, true)
	case stString:
		s.stringByte(b, false)
	}
}

func (s *Screen) ground(b byte) {
	// A control byte always aborts a partial UTF-8 sequence.
	if b < 0x20 || b == 0x7f {
		s.utf8Buf = s.utf8Buf[:0]
		switch b {
		case 0x07: // BEL
			if s.bells < 1<<20 {
				s.bells++
			}
		case 0x08: // BS
			s.wrapPending = false
			if s.x > 0 {
				s.x--
			}
		case 0x09: // HT
			s.tab(1)
		case 0x0a, 0x0b, 0x0c: // LF VT FF
			s.lineFeed()
		case 0x0d: // CR
			s.wrapPending = false
			s.x = 0
		case 0x1b:
			s.state = stEsc
			s.params = s.params[:0]
			s.interm = s.interm[:0]
		}
		// SO/SI/DEL and the rest are consumed and dropped.
		return
	}

	if b < 0x80 {
		s.utf8Buf = s.utf8Buf[:0]
		s.put(rune(b))
		return
	}

	// Multi-byte UTF-8: accumulate until a full rune decodes. Bounded at 4
	// bytes; anything longer or invalid becomes U+FFFD so malformed input can
	// never desynchronise the grid.
	s.utf8Buf = append(s.utf8Buf, b)
	if !utf8.FullRune(s.utf8Buf) {
		if len(s.utf8Buf) >= utf8.UTFMax {
			s.utf8Buf = s.utf8Buf[:0]
			s.put(utf8.RuneError)
		}
		return
	}
	r, _ := utf8.DecodeRune(s.utf8Buf)
	s.utf8Buf = s.utf8Buf[:0]
	// C1 controls arriving as UTF-8 are dropped rather than printed: they are
	// the classic way to smuggle a CSI past a naive filter.
	if r >= 0x80 && r <= 0x9f {
		return
	}
	s.put(r)
}

func (s *Screen) escape(b byte) {
	switch {
	case b == '[':
		s.state = stCSI
		s.params = s.params[:0]
		s.interm = s.interm[:0]
		return
	case b == ']':
		s.state = stOSC
		s.strBuf = s.strBuf[:0]
		s.strTrunc = false
		s.escPrev = 0
		return
	case b == 'P' || b == 'X' || b == '^' || b == '_':
		// DCS / SOS / PM / APC. Consumed to the terminator and discarded —
		// nothing in these payloads is ever rendered or re-emitted.
		s.state = stString
		s.strBuf = s.strBuf[:0]
		s.strTrunc = false
		s.escPrev = 0
		return
	case b >= 0x20 && b <= 0x2f:
		// Intermediate: charset designator ESC ( B, line attributes ESC # 8, …
		s.state = stEscInterm
		return
	}

	s.state = stGround
	switch b {
	case '7':
		s.saveCursor()
	case '8':
		s.restoreCursor()
	case 'c':
		s.Reset()
	case 'D': // IND
		s.index()
	case 'E': // NEL
		s.x = 0
		s.wrapPending = false
		s.index()
	case 'M': // RI
		s.reverseIndex()
	case 'H': // HTS
		s.tabs[s.x] = true
	}
	// ESC = / ESC > (keypad modes) and everything else: consumed.
}

// stringByte accumulates an OSC or DCS payload until its terminator (BEL for
// OSC, or ST = ESC \ for either). The payload is bounded; past the cap it is
// still consumed so the terminator resynchronises the parser, but the content
// is discarded.
func (s *Screen) stringByte(b byte, osc bool) {
	if s.escPrev == 0x1b {
		s.escPrev = 0
		if b == '\\' { // ST
			s.endString(osc)
			return
		}
		// ESC followed by anything else inside a string: abandon the string and
		// re-dispatch the byte as a fresh escape.
		s.state = stEsc
		s.escape(b)
		return
	}
	if b == 0x1b {
		s.escPrev = 0x1b
		return
	}
	if osc && b == 0x07 { // BEL terminates OSC
		s.endString(true)
		return
	}
	if len(s.strBuf) >= maxStringPayload {
		s.strTrunc = true
		return
	}
	s.strBuf = append(s.strBuf, b)
}

func (s *Screen) endString(osc bool) {
	if osc && !s.strTrunc {
		s.osc(string(s.strBuf))
	}
	s.strBuf = s.strBuf[:0]
	s.strTrunc = false
	s.state = stGround
}

// osc handles the only OSC commands we care about: the window title. Everything
// else — notably OSC 52, which writes the system clipboard — is deliberately
// ignored. The title is kept for display in bridge chrome and is sanitized
// there; it is never forwarded to the real terminal.
func (s *Screen) osc(payload string) {
	code, rest, ok := strings.Cut(payload, ";")
	if !ok {
		return
	}
	switch code {
	case "0", "2":
		s.title = boundedPrintable(rest, 120)
	}
}

// boundedPrintable keeps printable runes only and truncates. Defence in depth:
// callers sanitize again before rendering.
func boundedPrintable(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || r == utf8.RuneError {
			continue
		}
		if n >= max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

func (s *Screen) csi(b byte) {
	switch {
	case b >= 0x30 && b <= 0x3f: // parameter bytes (digits ; : < = > ?)
		if len(s.params) < maxParams*8 {
			s.params = append(s.params, b)
		}
		return
	case b >= 0x20 && b <= 0x2f: // intermediate bytes
		if len(s.interm) < 4 {
			s.interm = append(s.interm, b)
		}
		return
	case b >= 0x40 && b <= 0x7e: // final byte
		s.state = stGround
		s.dispatchCSI(b)
		return
	default:
		// A control byte inside a CSI: abandon it and handle the control.
		s.state = stGround
		s.ground(b)
	}
}

// csiParams splits the collected parameter bytes into numbers, dropping any
// private-marker prefix. Colon subparameters are flattened; SGR handles the
// colon forms it needs separately from the raw string.
func (s *Screen) csiParams() (private byte, nums []int) {
	raw := s.params
	if len(raw) > 0 && (raw[0] == '?' || raw[0] == '>' || raw[0] == '<' || raw[0] == '=') {
		private = raw[0]
		raw = raw[1:]
	}
	if len(raw) == 0 {
		return private, nil
	}
	for _, part := range strings.FieldsFunc(string(raw), func(r rune) bool { return r == ';' || r == ':' }) {
		if len(nums) >= maxParams {
			break
		}
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			n = 0
		}
		nums = append(nums, min(n, maxParamValue))
	}
	// Preserve empty leading/trailing params as 0 so "CSI ;5H" behaves.
	return private, nums
}

// param returns nums[i] defaulting to def when missing or zero.
func param(nums []int, i, def int) int {
	if i >= len(nums) || nums[i] == 0 {
		return def
	}
	return nums[i]
}

func (s *Screen) dispatchCSI(final byte) {
	private, nums := s.csiParams()
	// Intermediates we do not implement (DECSCUSR "CSI Ps q" is harmless to
	// ignore, DECSTR "CSI ! p" resets) — only handle the reset.
	if len(s.interm) > 0 {
		if s.interm[0] == '!' && final == 'p' {
			s.Reset()
		}
		return
	}

	switch final {
	case 'A': // CUU
		s.wrapPending = false
		s.y = max(s.scrollTop(), s.y-param(nums, 0, 1))
	case 'B', 'e': // CUD, VPR
		s.wrapPending = false
		s.y = min(s.scrollBot(), s.y+param(nums, 0, 1))
	case 'C', 'a': // CUF, HPR
		s.wrapPending = false
		s.x = min(s.cols-1, s.x+param(nums, 0, 1))
	case 'D': // CUB
		s.wrapPending = false
		s.x = max(0, s.x-param(nums, 0, 1))
	case 'E': // CNL
		s.wrapPending = false
		s.x = 0
		s.y = min(s.scrollBot(), s.y+param(nums, 0, 1))
	case 'F': // CPL
		s.wrapPending = false
		s.x = 0
		s.y = max(s.scrollTop(), s.y-param(nums, 0, 1))
	case 'G', '`': // CHA, HPA
		s.wrapPending = false
		s.x = clamp(param(nums, 0, 1)-1, 0, s.cols-1)
	case 'd': // VPA
		s.wrapPending = false
		s.y = s.absRow(param(nums, 0, 1) - 1)
	case 'H', 'f': // CUP, HVP
		s.wrapPending = false
		s.y = s.absRow(param(nums, 0, 1) - 1)
		s.x = clamp(param(nums, 1, 1)-1, 0, s.cols-1)
	case 'I': // CHT
		s.tab(param(nums, 0, 1))
	case 'Z': // CBT
		s.backTab(param(nums, 0, 1))
	case 'J': // ED
		s.eraseDisplay(paramRaw(nums, 0))
	case 'K': // EL
		s.eraseLine(paramRaw(nums, 0))
	case 'L': // IL
		s.insertLines(param(nums, 0, 1))
	case 'M': // DL
		s.deleteLines(param(nums, 0, 1))
	case '@': // ICH
		s.insertChars(param(nums, 0, 1))
	case 'P': // DCH
		s.deleteChars(param(nums, 0, 1))
	case 'X': // ECH
		s.eraseChars(param(nums, 0, 1))
	case 'S': // SU
		s.scrollUp(param(nums, 0, 1))
	case 'T': // SD
		s.scrollDown(param(nums, 0, 1))
	case 'g': // TBC
		if paramRaw(nums, 0) == 3 {
			s.tabs = map[int]bool{}
		} else {
			delete(s.tabs, s.x)
		}
	case 'r': // DECSTBM
		top := clamp(param(nums, 0, 1)-1, 0, s.rows-1)
		bot := clamp(param(nums, 1, s.rows)-1, 0, s.rows-1)
		if top < bot {
			s.top, s.bot = top, bot
		} else {
			s.top, s.bot = 0, s.rows-1
		}
		s.x = 0
		s.y = s.scrollTop()
		s.wrapPending = false
	case 'h':
		s.setModes(private, nums, true)
	case 'l':
		s.setModes(private, nums, false)
	case 'm':
		s.sgr()
	case 'n': // DSR
		if private == 0 {
			switch paramRaw(nums, 0) {
			case 5:
				s.reply("\x1b[0n")
			case 6:
				s.reply("\x1b[" + strconv.Itoa(s.y+1) + ";" + strconv.Itoa(s.x+1) + "R")
			}
		}
	case 'c': // DA
		if private == 0 {
			// VT100 with advanced video option — the same conservative answer
			// xterm.js gives, so a TUI probing for capabilities gets a reply
			// instead of hanging.
			s.reply("\x1b[?1;2c")
		} else if private == '>' {
			s.reply("\x1b[>0;0;0c")
		}
	case 's': // DECSC (ANSI.SYS form)
		s.saveCursor()
	case 'u': // DECRC (ANSI.SYS form)
		s.restoreCursor()
	}
	// Any other final byte is consumed and dropped.
}

// paramRaw returns nums[i] with a zero default (for selectors like ED/EL where
// 0 is meaningful).
func paramRaw(nums []int, i int) int {
	if i >= len(nums) {
		return 0
	}
	return nums[i]
}

func (s *Screen) reply(str string) {
	if len(s.replies)+len(str) > maxReplies {
		return
	}
	s.replies = append(s.replies, str...)
}

// absRow maps a row parameter to an absolute row, honoring origin mode.
func (s *Screen) absRow(r int) int {
	if s.originMode {
		return clamp(s.scrollTop()+r, s.scrollTop(), s.scrollBot())
	}
	return clamp(r, 0, s.rows-1)
}

func (s *Screen) scrollTop() int { return clamp(s.top, 0, s.rows-1) }
func (s *Screen) scrollBot() int { return clamp(s.bot, 0, s.rows-1) }

func (s *Screen) setModes(private byte, nums []int, on bool) {
	for _, n := range nums {
		if private == '?' {
			s.modes[n] = on
			switch n {
			case 1: // DECCKM — application cursor keys; changes what we SEND
			case 6: // DECOM
				s.originMode = on
				s.x = 0
				s.y = s.scrollTop()
			case 7: // DECAWM
				s.autoWrap = on
				s.wrapPending = false
			case 25: // DECTCEM
				s.cursorVisible = on
			case 47, 1047, 1049:
				s.switchAlt(on, n == 1049)
			}
			continue
		}
		if n == 4 { // IRM
			s.insertMode = on
		}
	}
}

// switchAlt enters or leaves the alternate screen buffer. Mode 1049 also saves
// and restores the cursor and clears the alt buffer on entry, which is what
// full-screen TUIs rely on.
func (s *Screen) switchAlt(on, withCursor bool) {
	if on == s.onAlt {
		return
	}
	if on {
		if withCursor {
			s.savePrimary = savedCursor{s.x, s.y, s.attr, s.originMode}
		}
		s.onAlt = true
		if withCursor {
			s.alt = newGrid(s.cols, s.rows)
			s.x, s.y = 0, 0
		}
		return
	}
	s.onAlt = false
	if withCursor {
		s.alt = newGrid(s.cols, s.rows)
		s.x, s.y, s.attr, s.originMode = s.savePrimary.x, s.savePrimary.y, s.savePrimary.attr, s.savePrimary.origin
		s.clampCursor()
	}
	s.wrapPending = false
}

func (s *Screen) saveCursor() {
	sc := savedCursor{s.x, s.y, s.attr, s.originMode}
	if s.onAlt {
		s.saveAlt = sc
	} else {
		s.savePrimary = sc
	}
}

func (s *Screen) restoreCursor() {
	sc := s.savePrimary
	if s.onAlt {
		sc = s.saveAlt
	}
	s.x, s.y, s.attr, s.originMode = sc.x, sc.y, sc.attr, sc.origin
	s.clampCursor()
	s.wrapPending = false
}

// sgr applies a Select Graphic Rendition sequence. It parses the raw parameter
// bytes rather than the flattened numbers so that the colon-separated color
// forms ("38:2::1:2:3") are handled correctly.
func (s *Screen) sgr() {
	raw := string(s.params)
	if len(raw) > 0 && (raw[0] == '?' || raw[0] == '>' || raw[0] == '<' || raw[0] == '=') {
		return // private SGR: not ours
	}
	if strings.TrimSpace(raw) == "" {
		s.attr = Attr{}
		return
	}
	groups := strings.Split(raw, ";")
	for i := 0; i < len(groups); i++ {
		g := groups[i]
		if strings.Contains(g, ":") {
			// Self-contained colon form: everything is inside this group.
			parts := strings.Split(g, ":")
			code := atoiSafe(parts[0])
			if code == 38 || code == 48 {
				if c, ok := colorFromColonParts(parts[1:]); ok {
					s.applyColor(code, c)
				}
				continue
			}
			// e.g. "4:3" curly underline — treat the base code only.
			s.applySGRCode(code)
			continue
		}
		code := atoiSafe(g)
		if code == 38 || code == 48 {
			consumed, c, ok := colorFromSemiParts(groups[i+1:])
			i += consumed
			if ok {
				s.applyColor(code, c)
			}
			continue
		}
		s.applySGRCode(code)
	}
}

func atoiSafe(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 || n > maxParamValue {
		return -1
	}
	return n
}

func colorFromColonParts(parts []string) (Color, bool) {
	if len(parts) == 0 {
		return Color{}, false
	}
	switch atoiSafe(parts[0]) {
	case 5:
		if len(parts) >= 2 {
			return Color{Kind: ColorIndexed, Idx: uint8(clamp(atoiSafe(parts[1]), 0, 255))}, true
		}
	case 2:
		// "2:r:g:b" or "2:colorspace:r:g:b"
		rgb := parts[1:]
		if len(rgb) >= 4 {
			rgb = rgb[len(rgb)-3:]
		}
		if len(rgb) >= 3 {
			return Color{
				Kind: ColorRGB,
				R:    uint8(clamp(atoiSafe(rgb[0]), 0, 255)),
				G:    uint8(clamp(atoiSafe(rgb[1]), 0, 255)),
				B:    uint8(clamp(atoiSafe(rgb[2]), 0, 255)),
			}, true
		}
	}
	return Color{}, false
}

// colorFromSemiParts parses the semicolon form following a 38/48 code and
// reports how many following groups it consumed.
func colorFromSemiParts(rest []string) (consumed int, c Color, ok bool) {
	if len(rest) == 0 {
		return 0, Color{}, false
	}
	switch atoiSafe(rest[0]) {
	case 5:
		if len(rest) >= 2 {
			return 2, Color{Kind: ColorIndexed, Idx: uint8(clamp(atoiSafe(rest[1]), 0, 255))}, true
		}
		return 1, Color{}, false
	case 2:
		if len(rest) >= 4 {
			return 4, Color{
				Kind: ColorRGB,
				R:    uint8(clamp(atoiSafe(rest[1]), 0, 255)),
				G:    uint8(clamp(atoiSafe(rest[2]), 0, 255)),
				B:    uint8(clamp(atoiSafe(rest[3]), 0, 255)),
			}, true
		}
		return len(rest), Color{}, false
	}
	return 1, Color{}, false
}

func (s *Screen) applyColor(code int, c Color) {
	if code == 38 {
		s.attr.FG = c
	} else {
		s.attr.BG = c
	}
}

func (s *Screen) applySGRCode(code int) {
	switch {
	case code == 0:
		s.attr = Attr{}
	case code == 1:
		s.attr.Bold = true
	case code == 2:
		s.attr.Faint = true
	case code == 3:
		s.attr.Italic = true
	case code == 4:
		s.attr.Underline = true
	case code == 5 || code == 6:
		s.attr.Blink = true
	case code == 7:
		s.attr.Reverse = true
	case code == 8:
		s.attr.Hidden = true
	case code == 9:
		s.attr.Strike = true
	case code == 21 || code == 22:
		s.attr.Bold, s.attr.Faint = false, false
	case code == 23:
		s.attr.Italic = false
	case code == 24:
		s.attr.Underline = false
	case code == 25:
		s.attr.Blink = false
	case code == 27:
		s.attr.Reverse = false
	case code == 28:
		s.attr.Hidden = false
	case code == 29:
		s.attr.Strike = false
	case code >= 30 && code <= 37:
		s.attr.FG = Color{Kind: ColorIndexed, Idx: uint8(code - 30)}
	case code == 39:
		s.attr.FG = Color{}
	case code >= 40 && code <= 47:
		s.attr.BG = Color{Kind: ColorIndexed, Idx: uint8(code - 40)}
	case code == 49:
		s.attr.BG = Color{}
	case code >= 90 && code <= 97:
		s.attr.FG = Color{Kind: ColorIndexed, Idx: uint8(code-90) + 8}
	case code >= 100 && code <= 107:
		s.attr.BG = Color{Kind: ColorIndexed, Idx: uint8(code-100) + 8}
	}
}

// --- grid mutation ----------------------------------------------------------

func (s *Screen) put(r rune) {
	w := runewidth.RuneWidth(r)
	if w == 0 {
		// Combining mark: fold it onto the previous cell if there is room in
		// the rune slot. We keep one rune per cell, so a bare combining mark
		// with no base is dropped.
		return
	}
	if s.wrapPending && s.autoWrap {
		s.x = 0
		s.lineFeed()
		s.wrapPending = false
	}
	if w == 2 && s.x == s.cols-1 {
		// A double-width rune cannot straddle the right margin.
		if !s.autoWrap {
			return
		}
		s.x = 0
		s.lineFeed()
	}
	if s.insertMode {
		s.insertChars(w)
	}
	row := s.buf()[s.y]
	row[s.x] = Cell{Rune: r, Attr: s.attr}
	if w == 2 && s.x+1 < s.cols {
		row[s.x+1] = Cell{Attr: s.attr, Cont: true}
	}
	s.x += w
	if s.x > s.cols-1 {
		s.x = s.cols - 1
		s.wrapPending = true
	}
}

func (s *Screen) lineFeed() {
	if s.y == s.scrollBot() {
		s.scrollUp(1)
		return
	}
	if s.y < s.rows-1 {
		s.y++
	}
}

func (s *Screen) index() { s.lineFeed() }

func (s *Screen) reverseIndex() {
	if s.y == s.scrollTop() {
		s.scrollDown(1)
		return
	}
	if s.y > 0 {
		s.y--
	}
}

func (s *Screen) scrollUp(n int) {
	top, bot := s.scrollTop(), s.scrollBot()
	n = clamp(n, 0, bot-top+1)
	if n == 0 {
		return
	}
	buf := s.buf()
	for y := top; y+n <= bot; y++ {
		buf[y] = buf[y+n]
	}
	for y := bot - n + 1; y <= bot; y++ {
		buf[y] = s.blankRow()
	}
}

func (s *Screen) scrollDown(n int) {
	top, bot := s.scrollTop(), s.scrollBot()
	n = clamp(n, 0, bot-top+1)
	if n == 0 {
		return
	}
	buf := s.buf()
	for y := bot; y-n >= top; y-- {
		buf[y] = buf[y-n]
	}
	for y := top; y < top+n && y <= bot; y++ {
		buf[y] = s.blankRow()
	}
}

// blankRow allocates an erased row. Erasure carries the current background
// color, matching xterm behavior for background-colored clears.
func (s *Screen) blankRow() []Cell {
	row := make([]Cell, s.cols)
	fill := Cell{Attr: Attr{BG: s.attr.BG}}
	for i := range row {
		row[i] = fill
	}
	return row
}

func (s *Screen) eraseDisplay(mode int) {
	buf := s.buf()
	switch mode {
	case 0:
		s.eraseLine(0)
		for y := s.y + 1; y < s.rows; y++ {
			buf[y] = s.blankRow()
		}
	case 1:
		s.eraseLine(1)
		for y := 0; y < s.y; y++ {
			buf[y] = s.blankRow()
		}
	case 2, 3:
		for y := 0; y < s.rows; y++ {
			buf[y] = s.blankRow()
		}
	}
	s.wrapPending = false
}

func (s *Screen) eraseLine(mode int) {
	row := s.buf()[s.y]
	fill := Cell{Attr: Attr{BG: s.attr.BG}}
	switch mode {
	case 0:
		for x := s.x; x < s.cols; x++ {
			row[x] = fill
		}
	case 1:
		for x := 0; x <= s.x && x < s.cols; x++ {
			row[x] = fill
		}
	case 2:
		for x := 0; x < s.cols; x++ {
			row[x] = fill
		}
	}
	s.wrapPending = false
}

func (s *Screen) eraseChars(n int) {
	row := s.buf()[s.y]
	fill := Cell{Attr: Attr{BG: s.attr.BG}}
	for x := s.x; x < s.cols && x < s.x+n; x++ {
		row[x] = fill
	}
	s.wrapPending = false
}

func (s *Screen) insertChars(n int) {
	row := s.buf()[s.y]
	n = clamp(n, 0, s.cols-s.x)
	if n == 0 {
		return
	}
	copy(row[s.x+n:], row[s.x:s.cols-n])
	fill := Cell{Attr: Attr{BG: s.attr.BG}}
	for x := s.x; x < s.x+n; x++ {
		row[x] = fill
	}
}

func (s *Screen) deleteChars(n int) {
	row := s.buf()[s.y]
	n = clamp(n, 0, s.cols-s.x)
	if n == 0 {
		return
	}
	copy(row[s.x:], row[s.x+n:])
	fill := Cell{Attr: Attr{BG: s.attr.BG}}
	for x := s.cols - n; x < s.cols; x++ {
		row[x] = fill
	}
}

// insertLines / deleteLines operate inside the scroll region and are no-ops
// when the cursor sits outside it, per DEC behavior.
func (s *Screen) insertLines(n int) {
	top, bot := s.scrollTop(), s.scrollBot()
	if s.y < top || s.y > bot {
		return
	}
	n = clamp(n, 0, bot-s.y+1)
	buf := s.buf()
	for y := bot; y-n >= s.y; y-- {
		buf[y] = buf[y-n]
	}
	for y := s.y; y < s.y+n && y <= bot; y++ {
		buf[y] = s.blankRow()
	}
	s.x = 0
}

func (s *Screen) deleteLines(n int) {
	top, bot := s.scrollTop(), s.scrollBot()
	if s.y < top || s.y > bot {
		return
	}
	n = clamp(n, 0, bot-s.y+1)
	buf := s.buf()
	for y := s.y; y+n <= bot; y++ {
		buf[y] = buf[y+n]
	}
	for y := bot - n + 1; y <= bot; y++ {
		buf[y] = s.blankRow()
	}
	s.x = 0
}

func (s *Screen) nextTabStop(from int) int {
	if len(s.tabs) > 0 {
		for x := from + 1; x < s.cols; x++ {
			if s.tabs[x] {
				return x
			}
		}
		return s.cols - 1
	}
	x := (from/8 + 1) * 8
	return min(x, s.cols-1)
}

func (s *Screen) tab(n int) {
	s.wrapPending = false
	for i := 0; i < n && s.x < s.cols-1; i++ {
		s.x = s.nextTabStop(s.x)
	}
}

func (s *Screen) backTab(n int) {
	s.wrapPending = false
	for i := 0; i < n && s.x > 0; i++ {
		if len(s.tabs) > 0 {
			moved := false
			for x := s.x - 1; x >= 0; x-- {
				if s.tabs[x] {
					s.x = x
					moved = true
					break
				}
			}
			if !moved {
				s.x = 0
			}
			continue
		}
		s.x = max(0, (s.x-1)/8*8)
	}
}

// --- rendering --------------------------------------------------------------

// RenderOpts controls how Lines paints the grid.
type RenderOpts struct {
	// ShowCursor draws the cell under the cursor in reverse video. The bridge
	// enables it only for the focused pane, so background tabs don't show a
	// second cursor.
	ShowCursor bool
}

// Lines renders the visible grid as one string per row, with SGR sequences
// generated entirely by this package. Trailing default-attribute blanks are
// trimmed so the caller's renderer can pad instead.
func (s *Screen) Lines(opts RenderOpts) []string {
	out := make([]string, s.rows)
	cx, cy, cvis := s.x, s.y, s.cursorVisible
	drawCursor := opts.ShowCursor && cvis
	for y := 0; y < s.rows; y++ {
		out[y] = s.renderRow(y, drawCursor && y == cy, cx)
	}
	return out
}

func (s *Screen) renderRow(y int, withCursor bool, cx int) string {
	row := s.buf()[y]
	// Find the last column that must be painted.
	last := -1
	for x := s.cols - 1; x >= 0; x-- {
		if !row[x].blank() || !row[x].Attr.IsZero() {
			last = x
			break
		}
	}
	if withCursor && cx > last {
		last = cx
	}
	if last < 0 {
		return ""
	}

	var b strings.Builder
	cur := Attr{}
	dirty := false
	for x := 0; x <= last; x++ {
		c := row[x]
		if c.Cont {
			continue // right half of a wide rune, already emitted
		}
		a := c.Attr
		if withCursor && x == cx {
			a.Reverse = !a.Reverse
		}
		if a != cur {
			b.WriteString(a.SGR())
			cur = a
			dirty = dirty || !a.IsZero()
		}
		r := c.Rune
		if r == 0 {
			r = ' '
		}
		b.WriteRune(r)
	}
	if !cur.IsZero() || dirty {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// String renders the grid as plain text with no attributes — used by tests and
// by anything that needs the screen content without styling.
func (s *Screen) String() string {
	rows := make([]string, s.rows)
	for y := 0; y < s.rows; y++ {
		rows[y] = strings.TrimRight(s.RowText(y), " ")
	}
	return strings.Join(rows, "\n")
}

// RowText returns row y as plain text, space-padded to the full width.
func (s *Screen) RowText(y int) string {
	if y < 0 || y >= s.rows {
		return ""
	}
	var b strings.Builder
	for _, c := range s.buf()[y] {
		if c.Cont {
			continue
		}
		r := c.Rune
		if r == 0 {
			r = ' '
		}
		b.WriteRune(r)
	}
	return b.String()
}
