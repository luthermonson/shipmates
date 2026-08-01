package bridge

import (
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/lipgloss"

	"github.com/luthermonson/shipmates/internal/bridge/vt"
)

// Marker glyphs. ASCII-safe fallbacks are not attempted: every terminal that can
// run claude's own TUI renders these, and the alternative (guessing at terminal
// capability) is worse than a stray box character.
const (
	glyphWorking  = "●"
	glyphIdle     = "○"
	glyphBlocked  = "!"
	glyphDone     = "×"
	glyphActivity = "*"
	glyphFocus    = "▸"
)

// frameGlyphs is one pane border. There are two sets and they differ in WEIGHT,
// not colour: a light box means the bridge is reading your keys, a heavy box means
// the mate is. Weight survives a monochrome terminal, a colour-blind operator and
// a screenshot; colour does not, and "am I typing at the agent or at the bridge?"
// is the one question the operator must never have to guess at.
type frameGlyphs struct {
	tl, tr, bl, br, h, v string
}

var (
	frameNavigate = frameGlyphs{tl: "┌", tr: "┐", bl: "└", br: "┘", h: "─", v: "│"}
	frameType     = frameGlyphs{tl: "┏", tr: "┓", bl: "┗", br: "┛", h: "━", v: "┃"}
)

// Style palette. Adaptive colors so the bridge is readable on a light or dark
// SSH terminal without asking.
var (
	styleHeader = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.AdaptiveColor{Light: "#1b1b1b", Dark: "#eeeeee"})

	styleTabActive = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("232")).Background(lipgloss.Color("81"))

	styleTabIdle = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#555555", Dark: "#aaaaaa"})

	// An approval badge must survive a glance at a wall of tabs, so it gets the
	// loudest treatment in the palette and keeps it on background tabs.
	styleTabBlocked = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("231")).Background(lipgloss.Color("196"))

	styleAlert = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("231")).Background(lipgloss.Color("196"))

	styleFooter = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#666666", Dark: "#8a8a8a"})

	styleNotice = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#7a4a00", Dark: "#e8b04b"})

	styleError = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("203"))

	styleOverlay = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).Padding(0, 1)

	styleSelected = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("232")).Background(lipgloss.Color("214"))

	styleDim = lipgloss.NewStyle().
			Foreground(lipgloss.AdaptiveColor{Light: "#888888", Dark: "#777777"})

	// The pane frame. Colour reinforces the border weight; it never carries the
	// meaning on its own.
	styleFrameNavigate = lipgloss.NewStyle().
				Foreground(lipgloss.AdaptiveColor{Light: "#999999", Dark: "#666666"})

	styleFrameType = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("81"))

	// The mode word. Reverse video, so it reads as a badge rather than as prose,
	// and so it is still obvious with colour stripped.
	styleModeNavigate = lipgloss.NewStyle().Bold(true).
				Foreground(lipgloss.Color("232")).Background(lipgloss.Color("250"))

	styleModeType = lipgloss.NewStyle().Bold(true).
			Foreground(lipgloss.Color("232")).Background(lipgloss.Color("81"))
)

// keyMap documents the bindings for the help overlay. Nothing dispatches through
// key.Matches — onNavigateKey and onTypeKey switch on the key name directly, which
// is what lets Type mode pass a key through without first asking a matcher about
// it. These bindings exist so the help text has one source of truth.
//
// Every Navigate binding is a BARE key and every Type binding is either bare esc or
// in the alt+ namespace. There is no prefix; ^B belongs to the mate.
type keyMap struct {
	// Navigate
	Number  key.Binding
	Next    key.Binding
	Prev    key.Binding
	Focus   key.Binding
	Approve key.Binding
	Reclaim key.Binding
	Drop    key.Binding
	Refresh key.Binding
	Help    key.Binding
	Quit    key.Binding

	// Type
	Leave     key.Binding
	AltNumber key.Binding
	AltEsc    key.Binding
	PassThru  key.Binding
}

func newKeyMap() keyMap {
	b := func(keys, help string) key.Binding {
		return key.NewBinding(key.WithKeys(keys), key.WithHelp(keys, help))
	}
	return keyMap{
		Number:  b("1-9", "select mate"),
		Next:    b("→ l tab", "next mate"),
		Prev:    b("← h", "prev mate"),
		Focus:   b("enter / i", "type at this mate"),
		Approve: b("a", "approvals"),
		Reclaim: b("r", "reclaim keyboard"),
		Drop:    b("x", "drop this pane"),
		Refresh: b("f", "refresh"),
		Help:    b("?", "help"),
		Quit:    b("q", "quit"),

		Leave:     b("esc", "back to navigate"),
		AltNumber: b("alt+1-9", "switch mate, keep typing"),
		AltEsc:    b("alt+esc", "send a literal esc"),
		PassThru:  b("every other key", "goes to the mate (^b, ^c, ^d …)"),
	}
}

func (k keyMap) navigateHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Number, k.Next, k.Prev},
		{k.Focus, k.Approve, k.Refresh},
		{k.Reclaim, k.Drop, k.Help, k.Quit},
	}
}

func (k keyMap) typeHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.PassThru},
		{k.Leave, k.AltNumber},
		{k.AltEsc},
	}
}

// View composes the frame: tab bar, the pane inside a mode-weighted box (or an
// overlay, drawn inside the same box so opening one never resizes a mate), an
// optional approval alert bar, and the status line.
//
// Every non-pane string is sanitized; the pane is the emulator's own re-emitted
// grid. The old separate header row is gone — its title and approval count moved
// onto the box's top rule, which is where "what is this box and what mode is it
// in" belongs, and that bought back a row for the mate.
func (m *Model) View() string {
	if m.quitting {
		return ""
	}
	cols, rows := m.paneSize()

	var b strings.Builder
	b.WriteString(m.tabBarView())
	b.WriteByte('\n')

	b.WriteString(m.paneTopView(cols))
	b.WriteByte('\n')

	body := m.paneView(cols, rows)
	if ov := m.overlayView(cols, rows); ov != "" {
		body = ov
	}
	b.WriteString(m.framed(body, cols, rows))
	b.WriteByte('\n')

	b.WriteString(m.paneBottomView(cols))
	b.WriteByte('\n')

	if alert := m.alertText(); alert != "" {
		b.WriteString(styleAlert.Render(padTo(alert, m.width)))
		b.WriteByte('\n')
	}
	b.WriteString(m.statusView(m.width))
	return b.String()
}

// frame picks the border glyphs and their style for the current mode.
func (m *Model) frame() (frameGlyphs, lipgloss.Style) {
	if m.mode == modeType {
		return frameType, styleFrameType
	}
	return frameNavigate, styleFrameNavigate
}

// modeBadge is the mode word, reverse-video, with the persona it applies to.
func (m *Model) modeBadge() string {
	if m.mode == modeType {
		return styleModeType.Render(" TYPE ")
	}
	return styleModeNavigate.Render(" NAVIGATE ")
}

// paneTopView draws the box's top rule: mode badge, focused persona, and the
// global approval count on the right.
func (m *Model) paneTopView(cols int) string {
	g, st := m.frame()

	badge := m.modeBadge()
	var title string
	if p := m.currentPersona(); p != "" {
		sep := " " + glyphFocus + " "
		if m.mode != modeType {
			sep = " "
		}
		title = st.Render(sep + Chrome(p, 24) + " ")
	}
	var right string
	if n := len(m.pendings); n > 0 {
		right = styleAlert.Render(" "+glyphBlocked+" "+strconv.Itoa(n)+" waiting ") + st.Render(g.h)
	}

	// Assemble to exactly cols cells between the corners, dropping the least
	// important piece first when the window is too narrow to hold everything.
	lead := st.Render(g.h)
	for {
		used := lipgloss.Width(lead) + lipgloss.Width(badge) + lipgloss.Width(title) + lipgloss.Width(right)
		if used <= cols {
			fill := cols - used
			mid := lead + badge + title
			if fill > 0 {
				mid += st.Render(strings.Repeat(g.h, fill))
			}
			return st.Render(g.tl) + mid + right + st.Render(g.tr)
		}
		switch {
		case right != "":
			right = ""
		case title != "":
			title = ""
		default:
			// Only the badge left and it still does not fit: clip it.
			return st.Render(g.tl) + truncate(badge, cols) + st.Render(g.tr)
		}
	}
}

func (m *Model) paneBottomView(cols int) string {
	g, st := m.frame()
	return st.Render(g.bl + strings.Repeat(g.h, max(0, cols)) + g.br)
}

// framed puts the vertical border on both sides of every pane row. Rows are padded
// to exactly cols cells first, with padTo's ANSI-aware measurement, so a right
// border cannot be pushed off the line by a wide rune or pulled left by an SGR
// sequence.
func (m *Model) framed(body string, cols, rows int) string {
	g, st := m.frame()
	v := st.Render(g.v)
	lines := strings.Split(body, "\n")
	for len(lines) < rows {
		lines = append(lines, "")
	}
	if len(lines) > rows {
		lines = lines[:rows]
	}
	for i, l := range lines {
		lines[i] = v + padTo(l, cols) + v
	}
	return strings.Join(lines, "\n")
}

// tabBarView is the primary approval surface. A blocked mate is loud in the tab
// bar whether or not it is focused, because the requirement that matters most is
// that an operator looking at one pane cannot miss an approval on another.
func (m *Model) tabBarView() string {
	if len(m.tabs) == 0 {
		return styleTabIdle.Render(Chrome("no live mates — start one with `shipmates tell <persona> …`", m.width))
	}
	var parts []string
	for i, t := range m.tabs {
		label := m.tabLabel(i, t)
		switch {
		case len(m.byPersona[t.persona]) > 0:
			parts = append(parts, styleTabBlocked.Render(label))
		case i == m.active:
			parts = append(parts, styleTabActive.Render(label))
		default:
			parts = append(parts, styleTabIdle.Render(label))
		}
	}
	line := strings.Join(parts, " ")
	if lipgloss.Width(line) <= m.width {
		return line
	}
	// Overflow: keep the active tab and everything blocked visible, summarise
	// the rest. Truncating blind would hide exactly the badge that matters.
	var keep []string
	hidden := 0
	for i, t := range m.tabs {
		if i == m.active || len(m.byPersona[t.persona]) > 0 {
			keep = append(keep, parts[i])
			continue
		}
		hidden++
	}
	line = strings.Join(keep, " ")
	if hidden > 0 {
		line += styleTabIdle.Render(" +" + strconv.Itoa(hidden) + " more")
	}
	return truncate(line, m.width)
}

func (m *Model) tabLabel(i int, t *tab) string {
	name := Chrome(t.persona, 24)
	idx := ""
	if i < 9 {
		idx = strconv.Itoa(i+1) + ":"
	}
	mark := m.tabMark(t)
	return " " + idx + name + mark + " "
}

func (m *Model) tabMark(t *tab) string {
	if n := len(m.byPersona[t.persona]); n > 0 {
		return " " + glyphBlocked + strconv.Itoa(n)
	}
	switch {
	case t.exited:
		return " " + glyphDone
	case t.activity:
		return " " + glyphActivity
	case !t.attached:
		return " " + glyphIdle
	case t.status == "working":
		return " " + glyphWorking
	}
	return ""
}

// alertText is the one-line banner shown when an approval is waiting on a mate
// the operator is NOT currently looking at. It names the personas so a glance
// tells you where to go.
func (m *Model) alertText() string {
	if len(m.pendings) == 0 {
		return ""
	}
	cur := m.currentPersona()
	var others []string
	seen := map[string]bool{}
	for _, p := range m.pendings {
		if p.Persona == cur || seen[p.Persona] {
			continue
		}
		seen[p.Persona] = true
		n := len(m.byPersona[p.Persona])
		others = append(others, Chrome(p.Persona, 24)+"("+strconv.Itoa(n)+")")
	}
	if len(others) == 0 {
		// Approvals exist, but all for the focused mate. Still tell the operator
		// how to act on them.
		return " " + glyphBlocked + " " + strconv.Itoa(len(m.pendings)) +
			" approval(s) for " + Chrome(cur, 24) + " — " + m.approveHint() + " "
	}
	return " " + glyphBlocked + " approvals waiting on " + truncate(strings.Join(others, " "), max(10, m.width-40)) +
		" — " + m.approveHint() + " "
}

// approveHint names the keys that open the approvals list from here. `a` is a bare
// Navigate key, so from Type mode it takes an esc first — and the banner says so
// rather than leaving the operator to press a and watch it land in the agent.
func (m *Model) approveHint() string {
	if m.mode == modeType {
		return "esc then a to decide"
	}
	return "a to decide"
}

// paneView renders the focused persona's emulator, or a placeholder.
//
// This is the only place a mate's content reaches the screen, and it is NOT a
// pass-through: every line comes from vt.Screen.Lines, which re-emits characters
// plus SGR attributes that the emulator itself computed. Cursor moves, screen
// clears, title sets, clipboard writes and mode changes in the mate's byte stream
// acted on the virtual grid and stop there.
//
// The placeholder never says "not attached" and never asks the operator to press
// something to attach: selecting a mate attaches it, so the only honest states here
// are "connecting", "exited" and "this went wrong". The old model rendered a stale
// snapshot for an unattached tab, which looked live while silently discarding every
// keystroke — the single worst thing about it.
func (m *Model) paneView(cols, rows int) string {
	t := m.current()
	if t == nil {
		return m.placeholder(rows, "No live mates on this machine.",
			"Start one with `shipmates tell <persona> \"…\"` or `shipmates open <persona>`,",
			"then press f to refresh.")
	}
	if t.screen == nil {
		var msgs []string
		switch {
		case t.err != "":
			msgs = []string{
				"Could not reach " + Chrome(t.persona, 40) + ".",
				styleError.Render(t.err),
				"Select the mate again to retry.",
			}
		case t.exited:
			msgs = []string{Chrome(t.persona, 40) + " has exited."}
		default:
			msgs = []string{"connecting to " + Chrome(t.persona, 40) + "…"}
		}
		return m.placeholder(rows, msgs...)
	}

	// The cursor is the mate's own; showing it while the bridge owns the keyboard
	// would claim the keystrokes are going somewhere they are not.
	showCursor := m.overlay == overlayNone && m.mode == modeType && !t.locked
	lines := t.screen.Lines(vt.RenderOpts{ShowCursor: showCursor})
	if len(lines) > rows {
		lines = lines[:rows]
	}
	for len(lines) < rows {
		lines = append(lines, "")
	}
	if !t.attached && !t.exited {
		lines[0] = truncate(styleAlert.Render(" reconnecting… "), cols)
	}
	return strings.Join(lines, "\n")
}

func (m *Model) placeholder(rows int, msgs ...string) string {
	out := make([]string, rows)
	start := max(0, rows/2-len(msgs)/2)
	for i, s := range msgs {
		if start+i < rows {
			out[start+i] = styleDim.Render(s)
		}
	}
	return strings.Join(out, "\n")
}

// statusView is the second of the two independent signals for the current mode: the
// badge repeats here, in words, next to the keys that mode actually accepts. An
// operator who has looked away and come back reads either the border weight or this
// line and knows where their next keystroke goes.
func (m *Model) statusView(cols int) string {
	t := m.current()

	var left string
	if m.notice != "" {
		left = m.modeBadge() + " " + truncate(styleNotice.Render(m.notice), max(0, cols-14))
	} else {
		left = m.modeBadge() + " " + styleFooter.Render(m.modeHint(t))
	}

	var right string
	switch {
	case t == nil:
		right = styleDim.Render("no mates")
	case t.locked:
		right = styleError.Render(lockedHint)
	case t.exited:
		right = styleDim.Render("exited")
	case t.attaching || (!t.attached && t.err == ""):
		right = styleDim.Render("connecting…")
	case m.mode == modeType && t.writer:
		right = styleDim.Render("keys → " + Chrome(t.persona, 24))
	case t.attached:
		right = styleDim.Render(strconv.Itoa(len(m.tabs)) + " mates")
	}
	if m.statusErr != "" {
		right = styleError.Render("status: " + m.statusErr)
	}
	if m.pendingErr != "" {
		right = styleError.Render("pending: " + m.pendingErr)
	}
	if t != nil && t.err != "" {
		right = styleError.Render(t.err)
	}
	return joinEnds(left, right, cols)
}

// modeHint is the short key list for the current mode. It is not a full help
// screen; it is the two or three things an operator needs without pressing ?.
func (m *Model) modeHint(t *tab) string {
	if m.mode == modeType {
		return "esc navigate · alt+1-9 switch mate · every other key goes to the mate"
	}
	focus := "enter type"
	if t != nil {
		focus = "enter type at " + Chrome(t.persona, 20)
	}
	return "1-9 select · " + focus + " · a approvals · ? help · q quit"
}

// --- overlays ---------------------------------------------------------------

func (m *Model) overlayView(cols, rows int) string {
	switch m.overlay {
	case overlayApprovals:
		return m.approvalsView(cols, rows)
	case overlayHelp:
		return m.helpView(cols, rows)
	case overlayAttachAsk:
		return m.askView(cols, rows)
	}
	return ""
}

// approvalsView lists every pending request with the tool and its argument
// summary. The summary is agent-authored text (a Bash command, a file path), so
// it arrived through ChromeLine and is additionally truncated to the box width.
func (m *Model) approvalsView(cols, rows int) string {
	inner := max(20, min(cols-4, 100))
	var lines []string
	lines = append(lines, styleHeader.Render("approvals — "+strconv.Itoa(len(m.pendings))+" waiting"))
	lines = append(lines, "")
	for i, p := range m.pendings {
		// Agent-authored: sanitize here, at the render boundary.
		head := Chrome(p.Persona, 40) + " wants " + Chrome(p.Tool, 40)
		var row string
		if i == m.approvalIdx {
			row = styleSelected.Render("> " + truncate(head, inner-2))
		} else {
			row = truncate("  "+head, inner)
		}
		lines = append(lines, row)
		if in := ChromeLine(p.Input, 400); in != "" {
			lines = append(lines, styleDim.Render(truncate("    "+in, inner)))
		}
	}
	lines = append(lines, "")
	lines = append(lines, styleFooter.Render(
		"j/k move · a allow · A allow "+timeBoxDuration+" · d deny · D deny all for mate · g go to pane · esc close"))
	return m.boxed(lines, inner, rows)
}

func (m *Model) helpView(cols, rows int) string {
	inner := max(20, min(cols-4, 80))
	lines := []string{styleHeader.Render("the bridge"), ""}
	lines = append(lines,
		styleDim.Render("Light border "+frameNavigate.v+" = the bridge has your keys."),
		styleDim.Render("Heavy border "+frameType.v+" = the mate has them."),
		"",
		styleModeNavigate.Render(" NAVIGATE ")+"  "+styleDim.Render("nothing reaches the mate"))
	lines = append(lines, keyRows(m.keys.navigateHelp())...)
	lines = append(lines,
		"",
		styleModeType.Render(" TYPE ")+"  "+styleDim.Render("the mate has the keyboard"))
	lines = append(lines, keyRows(m.keys.typeHelp())...)
	lines = append(lines,
		"",
		styleDim.Render("There is no prefix key: ^b reaches the mate as a literal ^b."),
		styleDim.Render("Focusing a mate attaches it and claims its keyboard for you."),
		styleDim.Render("Marks: "+glyphBlocked+"n approvals · "+glyphWorking+" working · "+
			glyphActivity+" output · "+glyphIdle+" no stream · "+glyphDone+" exited"),
		styleDim.Render("writer id "+m.api.ClientID),
		"",
		styleFooter.Render("any key to close"))
	return m.boxed(lines, inner, rows)
}

func keyRows(rows [][]key.Binding) []string {
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		var parts []string
		for _, bnd := range row {
			h := bnd.Help()
			parts = append(parts, h.Key+"  "+styleDim.Render(h.Desc))
		}
		out = append(out, "  "+strings.Join(parts, "   "))
	}
	return out
}

// askView is the mid-turn consent prompt. It is deliberately NOT phrased as
// "attach?" — attaching is automatic. The question is whether to kill a headless
// process that is part-way through an answer, which is destructive and needs a human.
func (m *Model) askView(cols, rows int) string {
	inner := max(20, min(cols-4, 76))
	persona := Chrome(m.askPersona, 40)
	lines := []string{
		styleHeader.Render(persona + " is mid-turn with no terminal"),
		"",
		persona + " is " + Chrome(m.statusOf(m.askPersona), 20) +
			" as a headless process.",
		"",
		"Opening a terminal resumes the same session under a PTY, which",
		"retires that process mid-turn and loses the in-flight answer.",
		"",
		"  o    open a terminal now, losing the in-flight answer",
		"  w    wait for the turn to finish, then open one",
		"  esc  leave it alone",
	}
	return m.boxed(lines, inner, rows)
}

func (m *Model) statusOf(persona string) string {
	for _, st := range m.roster {
		if st.Persona == persona {
			return st.Status
		}
	}
	return "unknown"
}

// boxed centres a bordered box in the pane region, padding to exactly rows lines
// so the frame height never changes and no PTY resize is triggered.
func (m *Model) boxed(lines []string, inner, rows int) string {
	body := styleOverlay.Width(inner).Render(strings.Join(lines, "\n"))
	boxLines := strings.Split(body, "\n")
	if len(boxLines) > rows {
		boxLines = boxLines[:rows]
	}
	out := make([]string, rows)
	start := max(0, (rows-len(boxLines))/2)
	for i, l := range boxLines {
		if start+i < rows {
			out[start+i] = l
		}
	}
	return strings.Join(out, "\n")
}

// --- small layout helpers ---------------------------------------------------

// joinEnds puts left at the start of a width-wide line and right at its end.
func joinEnds(left, right string, width int) string {
	lw, rw := lipgloss.Width(left), lipgloss.Width(right)
	if width <= 0 {
		return left
	}
	if lw+rw+1 > width {
		if rw >= width {
			return truncate(right, width)
		}
		return truncate(left, width-rw) + right
	}
	return left + strings.Repeat(" ", width-lw-rw) + right
}

// truncate clips a styled string to width display cells. It measures with
// lipgloss (ANSI-aware) and cuts on rune boundaries outside escape sequences, so
// it can never split an SGR sequence and leak a partial escape.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	var b strings.Builder
	used := 0
	inEsc := false
	for _, r := range s {
		if inEsc {
			b.WriteRune(r)
			// SGR and CSI sequences end with a byte in @..~
			if r >= '@' && r <= '~' {
				inEsc = false
			}
			continue
		}
		if r == 0x1b {
			inEsc = true
			b.WriteRune(r)
			continue
		}
		w := lipgloss.Width(string(r))
		if used+w > width {
			break
		}
		b.WriteRune(r)
		used += w
	}
	b.WriteString("\x1b[0m")
	return b.String()
}

// padTo right-pads to width display cells so a background-coloured bar fills the
// line.
func padTo(s string, width int) string {
	w := lipgloss.Width(s)
	if w >= width {
		return truncate(s, width)
	}
	return s + strings.Repeat(" ", width-w)
}
