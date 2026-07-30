// Package bridge is "the bridge": a local terminal UI for the operator who is
// SSH'd into the VM or pod running a ship. It replaces the browser as the
// operator surface, tabbing between every persona live on the machine and
// driving each one through the coordination server's PTY endpoints.
//
// # Shape
//
// One Bubble Tea program owns the whole screen. Per persona it holds a tab with
// its own vt.Screen and its own long-lived SSE subscription, so switching tabs
// is a single integer assignment and never tears a session down. Two pollers run
// beside the streams: /status.json for the roster and /pending.json for
// approvals. Approvals are deliberately NOT derived from the PTY stream — an
// operator must be able to see that a background mate is blocked without having
// that mate's pane focused, which is the whole reason the badge lives in the tab
// bar and the alert bar sits above the status line.
//
// # Input model
//
// The bridge is a dashboard that contains terminals, not a multiplexer. There are
// exactly two modes and both are named on screen:
//
//	Navigate (the mode it launches in)
//	  1-9            select that mate
//	  ←/→ h/l tab    move between mates
//	  enter, i       focus the selected mate for typing (mode becomes Type)
//	  a ? q x r f    approvals, help, quit, drop this pane, reclaim keyboard, refresh
//
//	Type
//	  every key      goes to the mate, byte for byte, including ^B and ^C
//	  esc            back to Navigate
//	  alt+1-9        switch mates without leaving Type
//	  alt+esc        send a literal ESC to the mate
//
// There is NO prefix key. ^B is a Claude Code binding and must reach the pane as
// 0x02, so the bridge cannot own it. Everything the bridge does is either a bare
// key in Navigate or in the alt+ namespace, which Claude Code does not use.
//
// Selecting a mate attaches it and, in Type mode, claims the writer lease. The
// server's single-writer lock (internal/server/ptyproc.go) was built to arbitrate
// between several BROWSER viewers of the old web UI; for one operator on a pod
// there is nobody to arbitrate with, so the claim is silent and "takeover" is not
// a word the operator ever sees. A genuine 409 is reported in the status line with
// a retry key, not promoted to a mode.
//
// # Trust boundary
//
// Agent output is untrusted. Inside the terminal pane, PTY bytes are interpreted
// by internal/bridge/vt into a fixed cell grid and re-emitted from a whitelist
// of attributes this code computes; no byte from a mate ever reaches the
// operator's real terminal verbatim. Outside the pane — tab labels, persona
// names, approval tool names and command summaries, status and error text —
// every string passes through Chrome/ChromeLine, which strips controls, C1, and
// bidi overrides, and bounds length. See sanitize.go.
package bridge

import (
	"context"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/luthermonson/shipmates/internal/bridge/vt"
)

// Poll cadences. Approvals poll faster than the roster because a blocked mate is
// what the operator is waiting on, and the server's own permission gate gives up
// after 110 seconds — a slow poll would burn a meaningful slice of that.
const (
	defaultPendingInterval = 1 * time.Second
	defaultStatusInterval  = 3 * time.Second

	// tickInterval is the model's heartbeat. Polls are scheduled off it so
	// there is exactly one timer, and so tests can step time by sending a
	// single message.
	tickInterval = 500 * time.Millisecond
)

// timeBoxDuration is the grant length for a "allow and stop asking" approval.
// Matches the durations `shipmates allow --for` is normally used with.
const timeBoxDuration = "30m"

// msgQueue bounds frames in flight from the stream goroutines. Sends block when
// it is full, which backpressures into the server's per-subscriber channel —
// and that already drops the oldest frame rather than stalling the mate.
const msgQueue = 512

// lockedHint is the whole of what a refused claim looks like: one line naming the
// retry key. The old model turned a 409 into a persistent "VIEW-ONLY" mode with a
// prefix chord to escape it; for a single operator on a pod there is nobody to
// arbitrate with, so it is a transient problem, not a state to live in.
const lockedHint = "another viewer holds the keyboard — r to retry"

// maxTabs caps the tab bar. A machine with an unreasonable roster degrades to
// "the first N plus anything attached or blocked" rather than an unbounded
// render.
const maxTabs = 64

// Options configures a bridge.
type Options struct {
	// Base is the coordination server's URL (loopback). Required.
	Base string

	// Focus is the persona to select at startup. Empty selects the first tab
	// that has a pending approval, else the first tab.
	Focus string

	// Bell receives a BEL byte when a NEW approval appears. It is deliberately
	// separate from the mates' own output: a BEL inside agent output is ignored
	// (an agent must not be able to ring the operator's terminal at will), and
	// only the bridge's own approval alert is audible. nil disables the bell.
	Bell io.Writer

	StatusInterval  time.Duration
	PendingInterval time.Duration
}

type overlayKind uint8

const (
	overlayNone overlayKind = iota
	overlayApprovals
	overlayHelp
	overlayAttachAsk
)

// inputMode is the two-state input machine. It is the single fact that decides
// where a keystroke goes, and it is rendered as a word in the status line and as
// the weight of the pane border, so it can never be ambiguous on screen.
type inputMode uint8

const (
	// modeNavigate is the launch mode: bare keys are bridge commands and nothing
	// reaches the mate.
	modeNavigate inputMode = iota
	// modeType passes every keystroke to the focused mate. Only esc (leave) and
	// alt+1-9 (switch mate) are intercepted.
	modeType
)

func (m inputMode) String() string {
	if m == modeType {
		return "TYPE"
	}
	return "NAVIGATE"
}

// tab is one persona's pane: its emulator, its stream, and the lock/attach state
// the status line reports.
type tab struct {
	persona string

	status string // last value from /status.json
	gone   bool   // dropped out of the roster while we kept the tab open

	screen *vt.Screen

	attached  bool // an SSE subscription is live
	attaching bool // a probe/start is in flight
	awaitIdle bool // operator chose "wait for idle" before attaching
	exited    bool // the mate's process ended

	// tried records that an automatic attach has already been attempted, so a
	// mate that refuses to attach doesn't get retried on every status poll.
	// Operator-driven attaches ignore it.
	tried bool

	// locked is set when the server answered 409 to our input: another viewer
	// holds the writer lease. writer is set once one of our writes succeeded.
	locked bool
	writer bool

	// activity marks unseen screen output on a background tab.
	activity bool

	err string

	// pipe serializes writes to this mate, so keystrokes reach the PTY in the
	// order they were typed. See input.go.
	pipe *inputPipe

	cancel context.CancelFunc

	// cols/rows we last told the server. Tracked so a burst of window resizes
	// doesn't turn into a burst of PTY resizes.
	cols, rows int
}

// Model is the bridge. Update is a pure function of (Model, Msg) — every side
// effect is expressed as a tea.Cmd — which is what makes the whole surface
// testable without a TTY.
type Model struct {
	api  *API
	opts Options

	// ctx bounds every stream goroutine; cancelled on quit.
	ctx    context.Context
	cancel context.CancelFunc

	// msgs carries frames from the stream goroutines into the Bubble Tea loop.
	msgs chan any

	// readers counts queue-reader commands currently parked on msgs waiting for a
	// frame. Production never looks at it; the test harness subtracts it from the
	// number of commands in flight so it can tell "blocked waiting for a mate to
	// say something" apart from "still finishing a request", and so settle to a
	// real quiescent point instead of guessing with a timeout.
	readers atomic.Int32

	width, height int

	tabs   []*tab
	active int

	roster []MateStatus

	pendings   []Pending
	byPersona  map[string][]Pending
	seen       map[string]bool
	pendingErr string
	statusErr  string

	// mode is Navigate or Type. See inputMode.
	mode inputMode

	overlay      overlayKind
	approvalIdx  int
	askPersona   string // persona awaiting confirmation to start a PTY mid-turn
	notice       string // transient one-line message in the status line
	lastNoticeAt time.Time

	// keylog is temporary, env-gated input instrumentation. nil unless
	// SHIPMATES_BRIDGE_KEYLOG is set. See keylog.go.
	keylog *keyLog

	keys keyMap

	nextStatusPoll  time.Time
	nextPendingPoll time.Time
	now             time.Time

	quitting bool
}

// New builds a bridge model. The returned model has not contacted the server;
// Init issues the first polls.
func New(opts Options) *Model {
	if opts.StatusInterval <= 0 {
		opts.StatusInterval = defaultStatusInterval
	}
	if opts.PendingInterval <= 0 {
		opts.PendingInterval = defaultPendingInterval
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Model{
		api:       NewAPI(opts.Base),
		opts:      opts,
		ctx:       ctx,
		cancel:    cancel,
		msgs:      make(chan any, msgQueue),
		byPersona: map[string][]Pending{},
		seen:      map[string]bool{},
		keylog:    openKeyLog(),
		keys:      newKeyMap(),
		// Navigate is the launch mode: bare 1-9 must work the instant the bridge
		// opens, and no keystroke may reach a mate before the operator asks for it.
		mode: modeNavigate,
		// Sizes are placeholders until the first WindowSizeMsg; nothing renders
		// before then.
		width:  80,
		height: 24,
		now:    time.Now(),
	}
}

// ClientID exposes the writer-lock identity, for tests and for the footer.
func (m *Model) ClientID() string { return m.api.ClientID }

// --- messages ---------------------------------------------------------------

type tickMsg time.Time

type statusMsg struct {
	List []MateStatus
	Err  error
}

type pendingMsg struct {
	List []Pending
	Err  error
}

// probeAttachMsg is the answer to "does this persona already have a PTY?".
// A nil Err means yes and Snapshot holds its ring buffer.
type probeAttachMsg struct {
	Persona  string
	Snapshot []byte
	Err      error
}

// attachedMsg reports the result of starting a PTY mate and priming from its
// snapshot.
type attachedMsg struct {
	Persona  string
	Snapshot []byte
	Err      error
}

type ptyFrameMsg struct {
	Persona string
	Kind    string
	Data    []byte
}

type ptyStreamEndMsg struct {
	Persona string
	Exited  bool
	Err     error
}

type inputResultMsg struct {
	Persona string
	Err     error
}

type resolveResultMsg struct {
	ID       string
	Behavior string
	Err      error
}

type simpleResultMsg struct {
	Persona string
	What    string
	Err     error
}

// --- lifecycle --------------------------------------------------------------

// Init starts the heartbeat, the first polls, and the reader that drains stream
// goroutines into the update loop.
func (m *Model) Init() tea.Cmd {
	return tea.Batch(
		m.readQueue(),
		m.fetchStatus(),
		m.fetchPending(),
		tick(),
	)
}

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// readQueue delivers the next message from the stream goroutines. It is re-armed
// every time one is consumed, which is the standard way to bridge a channel into
// Bubble Tea.
func (m *Model) readQueue() tea.Cmd {
	ctx, ch := m.ctx, m.msgs
	return func() tea.Msg {
		m.readers.Add(1)
		defer m.readers.Add(-1)
		select {
		case v := <-ch:
			return v
		case <-ctx.Done():
			return nil
		}
	}
}

func (m *Model) fetchStatus() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		list, err := api.Status(context.Background())
		return statusMsg{List: list, Err: err}
	}
}

func (m *Model) fetchPending() tea.Cmd {
	api := m.api
	return func() tea.Msg {
		list, err := api.PendingList(context.Background())
		return pendingMsg{List: list, Err: err}
	}
}

// Update is the whole state machine.
//
// It delegates to update and then reconciles the pane geometry, once, for every
// message. Window size is not the only thing that moves the pane: the approval
// alert bar is a row that appears and disappears as mates get blocked and
// unblocked, and the focused persona decides whether that row is drawn at all. Any
// of those shrinks the pane, and a mate that isn't told has its bottom line
// silently clipped. Reconciling centrally means no future branch can forget to.
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	cmd := m.update(msg)
	if m.quitting {
		// On the way out the tabs are being released; resizing them now would race
		// the release and hand the next client a stale geometry.
		return m, cmd
	}
	return m, tea.Batch(cmd, m.reflow())
}

func (m *Model) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		return m.onResize(msg)

	case tea.KeyMsg:
		return m.onKey(msg)

	case tickMsg:
		m.now = time.Time(msg)
		var cmds []tea.Cmd
		if !m.now.Before(m.nextStatusPoll) {
			m.nextStatusPoll = m.now.Add(m.opts.StatusInterval)
			cmds = append(cmds, m.fetchStatus())
		}
		if !m.now.Before(m.nextPendingPoll) {
			m.nextPendingPoll = m.now.Add(m.opts.PendingInterval)
			cmds = append(cmds, m.fetchPending())
		}
		if !m.lastNoticeAt.IsZero() && m.now.Sub(m.lastNoticeAt) > 6*time.Second {
			m.notice = ""
			m.lastNoticeAt = time.Time{}
		}
		cmds = append(cmds, tick())
		return tea.Batch(cmds...)

	case statusMsg:
		return m.onStatus(msg)

	case pendingMsg:
		return m.onPending(msg)

	case probeAttachMsg:
		return m.onProbe(msg)

	case attachedMsg:
		return m.onAttached(msg)

	case ptyFrameMsg:
		return tea.Batch(m.onFrame(msg), m.readQueue())

	case ptyStreamEndMsg:
		return tea.Batch(m.onStreamEnd(msg), m.readQueue())

	case inputResultMsg:
		t := m.tab(msg.Persona)
		if t == nil {
			return nil
		}
		switch {
		case msg.Err == nil:
			t.locked = false
			t.writer = true
			t.err = ""
		case errors.Is(msg.Err, ErrLocked):
			// The one place a 409 can still surface: the server accepted our claim
			// but somebody else re-claimed before the keystroke landed. Report it
			// in the status line with a retry key; do NOT turn it into a mode.
			t.locked = true
			t.writer = false
			t.err = lockedHint
		case errors.Is(msg.Err, ErrNoPTY):
			t.attached = false
			t.err = "mate is gone — select it again to reconnect"
		default:
			t.err = ChromeLine(msg.Err.Error(), 120)
		}
		return nil

	case resolveResultMsg:
		if msg.Err != nil {
			m.setNotice("resolve failed: " + ChromeLine(msg.Err.Error(), 100))
		} else {
			m.setNotice(pastTense(msg.Behavior) + " " + Chrome(msg.ID, 16))
		}
		// Refresh immediately so the row disappears without waiting a tick.
		return m.fetchPending()

	case simpleResultMsg:
		t := m.tab(msg.Persona)
		if t == nil {
			return nil
		}
		switch {
		case msg.Err == nil:
			switch msg.What {
			case "claim":
				// Deliberately silent. Claiming the keyboard is a consequence of
				// focusing a mate, not an event the operator needs told about.
				t.locked = false
				t.writer = true
				if t.err == lockedHint {
					t.err = ""
				}
			case "release":
				t.writer = false
			}
		case errors.Is(msg.Err, ErrLocked):
			// Only a refused CLAIM is a keyboard problem. A 409 on resize means
			// another viewer's geometry wins, which the bridge accepts silently —
			// reporting it would put a scary line on screen for something the
			// operator neither caused nor can fix.
			if msg.What == "claim" {
				t.locked = true
				t.err = lockedHint
			}
		case errors.Is(msg.Err, ErrNoPTY):
			// resize/release/claim against a mate that just exited: not worth a
			// visible error, the stream-end message covers it.
		default:
			m.setNotice(msg.What + " failed: " + ChromeLine(msg.Err.Error(), 100))
		}
		return nil
	}
	return nil
}

// pastTense renders a resolve behavior for the footer. "deny"+"ed" is not a word.
func pastTense(behavior string) string {
	switch behavior {
	case "allow":
		return "allowed"
	case "deny":
		return "denied"
	}
	return Chrome(behavior, 20)
}

func (m *Model) setNotice(s string) {
	m.notice = Chrome(s, 200)
	m.lastNoticeAt = m.now
}

// --- resize -----------------------------------------------------------------

// onResize records the new window size. Bubble Tea delivers WindowSizeMsg on
// every platform, so there is no ioctl or ConPTY handling here; the actual
// reflow is done by reflow, which Update runs for every message.
func (m *Model) onResize(msg tea.WindowSizeMsg) tea.Cmd {
	m.width, m.height = msg.Width, msg.Height
	return nil
}

// reflow brings every emulator to the current pane geometry and tells the server
// about any ATTACHED mate whose geometry actually changed.
//
// Cheap to call unconditionally: vt.Screen.Resize returns immediately when the
// size is unchanged, and the cols/rows the server was last told are tracked per
// tab, so a burst of messages produces at most one PTY resize per real change.
func (m *Model) reflow() tea.Cmd {
	cols, rows := m.paneSize()
	var cmds []tea.Cmd
	for _, t := range m.tabs {
		if t.screen != nil {
			t.screen.Resize(cols, rows)
		}
		if t.attached && (t.cols != cols || t.rows != rows) {
			t.cols, t.rows = cols, rows
			cmds = append(cmds, m.resizeCmd(t.persona, cols, rows))
		}
	}
	return tea.Batch(cmds...)
}

// paneSize is the terminal pane's geometry: the window minus the tab-bar line, the
// pane box's two rules, the status line, and the alert line when it is showing;
// and minus the box's two vertical borders horizontally.
//
// Overlays draw INSIDE the pane region so opening one never resizes a mate.
func (m *Model) paneSize() (cols, rows int) {
	cols = m.width - 2  // │ … │
	rows = m.height - 4 // tab bar, top rule, bottom rule, status line
	if m.alertText() != "" {
		rows--
	}
	if cols < vt.MinCols {
		cols = vt.MinCols
	}
	if rows < vt.MinRows {
		rows = vt.MinRows
	}
	return cols, rows
}

func (m *Model) resizeCmd(persona string, cols, rows int) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		err := api.PTYResize(context.Background(), persona, cols, rows)
		return simpleResultMsg{Persona: persona, What: "resize", Err: err}
	}
}

// --- roster -----------------------------------------------------------------

// onStatus folds /status.json into the tab list. Tabs exist for every mate with
// a process (working/idle/blocked) plus any tab already open, so a mate that
// exits doesn't yank its scrollback out from under the operator.
func (m *Model) onStatus(msg statusMsg) tea.Cmd {
	if msg.Err != nil {
		m.statusErr = ChromeLine(msg.Err.Error(), 120)
		return nil
	}
	m.statusErr = ""
	m.roster = msg.List

	want := map[string]MateStatus{}
	for _, st := range msg.List {
		if st.Live() {
			want[st.Persona] = st
		}
	}

	focused := ""
	if t := m.current(); t != nil {
		focused = t.persona
	}

	// Update or retire existing tabs.
	kept := make([]*tab, 0, len(m.tabs))
	for _, t := range m.tabs {
		st, ok := want[t.persona]
		if ok {
			t.status = st.Status
			t.gone = false
			delete(want, t.persona)
			kept = append(kept, t)
			continue
		}
		// Not live any more. Keep the tab while it is attached or has content
		// worth reading; drop it once it is inert.
		t.gone = true
		if t.attached || t.screen != nil {
			kept = append(kept, t)
		}
	}
	// Add tabs for newly live mates.
	for _, st := range want {
		if len(kept) >= maxTabs {
			break
		}
		kept = append(kept, &tab{persona: st.Persona, status: st.Status})
	}
	sort.SliceStable(kept, func(i, j int) bool { return kept[i].persona < kept[j].persona })
	m.tabs = kept
	m.reindex(focused)

	// Honor a deferred attach: the operator asked to wait for a busy mate to
	// finish its turn before taking its session over.
	var cmds []tea.Cmd
	for _, t := range m.tabs {
		if t.awaitIdle && !t.attached && !t.attaching && !m.busy(t.persona) {
			t.awaitIdle = false
			t.attaching = true
			cmds = append(cmds, m.startAndPrime(t.persona))
		}
	}

	// First run: pick a focus. Prefer whatever needs a human.
	if m.active == 0 && focused == "" {
		m.focusInitial()
	}
	// Auto-attach the focused mate exactly once, so opening the bridge lands on
	// a live pane instead of an "attach?" prompt. `tried` keeps a mate that
	// can't attach from being retried on every poll.
	if t := m.current(); t != nil && !t.tried && !t.attached && !t.attaching && !t.gone && !t.exited {
		cmds = append(cmds, m.beginAttach(t))
	}
	return tea.Batch(cmds...)
}

func (m *Model) focusInitial() {
	if m.opts.Focus != "" {
		for i, t := range m.tabs {
			if t.persona == m.opts.Focus {
				m.active = i
				return
			}
		}
	}
	for i, t := range m.tabs {
		if len(m.byPersona[t.persona]) > 0 || t.status == "blocked" {
			m.active = i
			return
		}
	}
	m.active = 0
}

// reindex restores the active index after the tab slice is rebuilt.
func (m *Model) reindex(focused string) {
	if focused != "" {
		for i, t := range m.tabs {
			if t.persona == focused {
				m.active = i
				return
			}
		}
	}
	if m.active >= len(m.tabs) {
		m.active = max(0, len(m.tabs)-1)
	}
}

func (m *Model) busy(persona string) bool {
	for _, st := range m.roster {
		if st.Persona == persona {
			return st.Status == "working" || st.Status == "blocked"
		}
	}
	return false
}

// onPending folds /pending.json in and rings the bell for genuinely new
// requests.
//
// Values are stored RAW. Persona names and pending ids are server identifiers:
// they key the tab map and go into the /resolve/{id} path, so sanitizing them
// here would silently mis-key a badge or resolve the wrong request. Sanitization
// happens at exactly one place — the renderer (see view.go) — which is also the
// only place the strings can do harm.
func (m *Model) onPending(msg pendingMsg) tea.Cmd {
	if msg.Err != nil {
		m.pendingErr = ChromeLine(msg.Err.Error(), 120)
		return nil
	}
	m.pendingErr = ""

	clean := append([]Pending(nil), msg.List...)
	// Stable order so the cursor doesn't jump between polls.
	sort.SliceStable(clean, func(i, j int) bool {
		if clean[i].Persona != clean[j].Persona {
			return clean[i].Persona < clean[j].Persona
		}
		return clean[i].ID < clean[j].ID
	})

	fresh := false
	live := map[string]bool{}
	byPersona := map[string][]Pending{}
	for _, p := range clean {
		live[p.ID] = true
		if !m.seen[p.ID] {
			fresh = true
		}
		byPersona[p.Persona] = append(byPersona[p.Persona], p)
	}
	for id := range m.seen {
		if !live[id] {
			delete(m.seen, id)
		}
	}
	for id := range live {
		m.seen[id] = true
	}

	m.pendings = clean
	m.byPersona = byPersona
	if m.approvalIdx >= len(clean) {
		m.approvalIdx = max(0, len(clean)-1)
	}
	if fresh {
		return m.ringBell()
	}
	return nil
}

// ringBell writes a single BEL to the configured writer. This is the ONLY audible
// alert in the bridge: a BEL inside a mate's own output is counted and shown
// visually but never forwarded, so an agent cannot make the operator's terminal
// beep on demand.
func (m *Model) ringBell() tea.Cmd {
	w := m.opts.Bell
	if w == nil {
		return nil
	}
	return func() tea.Msg {
		_, _ = w.Write([]byte{0x07})
		return nil
	}
}

// --- attach -----------------------------------------------------------------

func (m *Model) tab(persona string) *tab {
	for _, t := range m.tabs {
		if t.persona == persona {
			return t
		}
	}
	return nil
}

func (m *Model) current() *tab {
	if m.active < 0 || m.active >= len(m.tabs) {
		return nil
	}
	return m.tabs[m.active]
}

// beginAttach probes for an existing PTY. That order matters: if the mate is
// already hosted under a PTY, attaching costs nothing and steals nothing, so it
// happens without asking. Only a 404 means attaching would spawn — and possibly
// kill a headless turn — which is where the operator gets a say.
func (m *Model) beginAttach(t *tab) tea.Cmd {
	if t == nil || t.attached || t.attaching {
		return nil
	}
	t.attaching = true
	t.tried = true
	t.err = ""
	api, persona := m.api, t.persona
	return func() tea.Msg {
		snap, err := api.PTYSnapshot(context.Background(), persona)
		return probeAttachMsg{Persona: persona, Snapshot: snap, Err: err}
	}
}

func (m *Model) onProbe(msg probeAttachMsg) tea.Cmd {
	t := m.tab(msg.Persona)
	if t == nil {
		return nil
	}
	switch {
	case msg.Err == nil:
		// A PTY already exists: prime and subscribe, no session takeover.
		return m.finishAttach(t, msg.Snapshot)
	case errors.Is(msg.Err, ErrNoPTY):
		if m.busy(t.persona) {
			// Carried over from the web client's attach grace period: starting
			// a PTY retires a mid-turn headless process and loses the in-flight
			// answer, so make the operator choose.
			t.attaching = false
			m.askPersona = t.persona
			m.overlay = overlayAttachAsk
			return nil
		}
		return m.startAndPrime(t.persona)
	default:
		t.attaching = false
		t.err = ChromeLine(msg.Err.Error(), 120)
		return nil
	}
}

// startAndPrime spawns the PTY mate and reads its first snapshot.
func (m *Model) startAndPrime(persona string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		if err := api.PTYStart(context.Background(), persona); err != nil {
			return attachedMsg{Persona: persona, Err: err}
		}
		snap, err := api.PTYSnapshot(context.Background(), persona)
		if err != nil && !errors.Is(err, ErrNoPTY) {
			return attachedMsg{Persona: persona, Err: err}
		}
		return attachedMsg{Persona: persona, Snapshot: snap}
	}
}

func (m *Model) onAttached(msg attachedMsg) tea.Cmd {
	t := m.tab(msg.Persona)
	if t == nil {
		return nil
	}
	if msg.Err != nil {
		t.attaching = false
		t.err = ChromeLine(msg.Err.Error(), 160)
		return nil
	}
	return m.finishAttach(t, msg.Snapshot)
}

// finishAttach primes the emulator from the snapshot so a freshly opened tab is
// never blank, then subscribes.
//
// The plain snapshot endpoint does NOT carry the latched DEC private mode
// prefix — only the stream's own snapshot event does — so this first paint can
// have the wrong modes. That is fine and intentional: the stream's snapshot
// arrives moments later, resets the screen, and re-primes with modes included.
// Priming twice costs one repaint and buys an instantly non-blank pane.
func (m *Model) finishAttach(t *tab, snapshot []byte) tea.Cmd {
	cols, rows := m.paneSize()
	if t.screen == nil {
		t.screen = vt.New(cols, rows)
	} else {
		t.screen.Reset()
		t.screen.Resize(cols, rows)
	}
	if len(snapshot) > 0 {
		_, _ = t.screen.Write(snapshot)
	}
	t.attaching = false
	t.attached = true
	t.exited = false
	t.err = ""
	t.cols, t.rows = cols, rows

	streamCtx, cancel := context.WithCancel(m.ctx)
	t.cancel = cancel

	api, persona, msgs := m.api, t.persona, m.msgs
	start := func() tea.Msg {
		go api.streamPTY(streamCtx, persona, msgs)
		return nil
	}
	cmds := []tea.Cmd{start, m.resizeCmd(t.persona, cols, rows)}
	// The operator pressed enter before this attach finished (or is typing at this
	// mate already). Claim now rather than making them press anything again.
	if m.mode == modeType && t == m.current() && !t.writer {
		cmds = append(cmds, m.claimCmd(t.persona))
	}
	return tea.Batch(cmds...)
}

// detach closes our subscription and hands the keyboard back, leaving the mate
// running. There is no endpoint to kill a PTY mate and the bridge does not try
// to fake one.
//
// It also drops out of Type mode: typing at a pane with no stream is exactly the
// "looks live and eats input" state this redesign removes.
func (m *Model) detach(t *tab) tea.Cmd {
	if t == nil {
		return nil
	}
	if t == m.current() {
		m.mode = modeNavigate
	}
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	wasAttached := t.attached
	t.attached = false
	t.attaching = false
	t.awaitIdle = false
	t.locked = false
	t.writer = false
	if t.pipe != nil {
		// Anything still queued was typed at a pane the operator has left.
		t.pipe.abort()
	}
	if !wasAttached {
		return nil
	}
	return m.releaseCmd(t.persona)
}

func (m *Model) releaseCmd(persona string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		err := api.PTYRelease(context.Background(), persona)
		return simpleResultMsg{Persona: persona, What: "release", Err: err}
	}
}

// --- frames -----------------------------------------------------------------

func (m *Model) onFrame(msg ptyFrameMsg) tea.Cmd {
	t := m.tab(msg.Persona)
	if t == nil || t.screen == nil {
		return nil
	}
	if msg.Kind == "snapshot" {
		// Reset first: the stream's snapshot is authoritative (mode prefix plus
		// ring) and must not stack on the pre-paint we did from the plain
		// snapshot endpoint.
		t.screen.Reset()
		cols, rows := m.paneSize()
		t.screen.Resize(cols, rows)
	}
	_, _ = t.screen.Write(msg.Data)

	// A mate's BEL is recorded, never forwarded — see ringBell.
	if t.screen.TakeBells() > 0 && t != m.current() {
		t.activity = true
	}
	if t != m.current() {
		t.activity = true
	}

	// Device reports (cursor position, device attributes) are answers the mate
	// is blocking on. Delivering them is a WRITE, and the server's writer lock
	// treats a write as a claim — so only the focused pane replies. A background
	// pane staying silent is strictly better than a background pane silently
	// stealing the operator's keyboard.
	replies := t.screen.TakeReplies()
	if len(replies) == 0 || t != m.current() || t.locked {
		return nil
	}
	return m.inputCmd(t, replies)
}

func (m *Model) onStreamEnd(msg ptyStreamEndMsg) tea.Cmd {
	t := m.tab(msg.Persona)
	if t == nil {
		return nil
	}
	t.attached = false
	t.attaching = false
	t.writer = false
	if t.cancel != nil {
		t.cancel()
		t.cancel = nil
	}
	// Typing into a pane whose stream has ended is the "looks live and eats input"
	// failure. Fall back to Navigate so the mode word on screen matches reality.
	if t == m.current() {
		m.mode = modeNavigate
	}
	switch {
	case msg.Exited:
		t.exited = true
		t.err = ""
		if t.screen != nil {
			_, _ = t.screen.WriteString("\r\n[mate exited]\r\n")
		}
	case msg.Err != nil:
		t.err = ChromeLine(msg.Err.Error(), 160)
	}
	return nil
}

// --- input ------------------------------------------------------------------

// inputCmd queues data for the mate and, when no drain is already running,
// returns the command that flushes the queue.
//
// It must be called from Update (never from inside a tea.Cmd): the ordering
// guarantee comes from Update being single-threaded. Chunking happens during the
// drain rather than up front, so a burst of keystrokes coalesces into as few
// requests as the server will take.
func (m *Model) inputCmd(t *tab, data []byte) tea.Cmd {
	if t == nil || len(data) == 0 {
		return nil
	}
	if t.pipe == nil {
		t.pipe = &inputPipe{}
	}
	start, dropped := t.pipe.enqueue(data)
	if dropped {
		m.setNotice("input backlog full for " + Chrome(t.persona, 40) + " — keystrokes dropped")
		return nil
	}
	if !start {
		return nil
	}
	api, persona, pipe := m.api, t.persona, t.pipe
	return func() tea.Msg {
		for {
			chunk := pipe.take(maxInputChunk)
			if chunk == nil {
				return inputResultMsg{Persona: persona}
			}
			if err := api.PTYInput(context.Background(), persona, chunk); err != nil {
				// The rest of the burst would fail identically (a held writer
				// lock, a dead mate), and sending a suffix of what was typed is
				// worse than sending none of it.
				pipe.abort()
				return inputResultMsg{Persona: persona, Err: err}
			}
		}
	}
}

// onKey is the input router.
//
// Order is deliberate. Overlays capture keys first — an approval prompt must never
// leak a keystroke into a mate — and then the mode decides everything else. There
// is no prefix layer: in Navigate a bare key is a command, in Type it is a
// keystroke for the mate, and nothing in between.
func (m *Model) onKey(msg tea.KeyMsg) tea.Cmd {
	switch m.overlay {
	case overlayHelp:
		m.overlay = overlayNone
		m.logKey(msg, nil, false)
		return nil
	case overlayAttachAsk:
		m.logKey(msg, nil, false)
		return m.onAskKey(msg)
	case overlayApprovals:
		m.logKey(msg, nil, false)
		return m.onApprovalKey(msg)
	}
	if m.mode == modeType {
		return m.onTypeKey(msg)
	}
	m.logKey(msg, nil, false)
	return m.onNavigateKey(msg)
}

// logKey feeds the temporary keylog. Called from Update (never from a tea.Cmd) so
// the file's line order is the order the keys were pressed.
func (m *Model) logKey(msg tea.KeyMsg, encoded []byte, sent bool) {
	m.keylog.record(msg, m.mode.String(), encoded, sent)
}

// onTypeKey is the pass-through path. EVERY key goes to the mate — ^B included,
// because Claude Code binds it — with exactly two exceptions:
//
//	esc       leaves Type mode. Claude Code also uses esc, so alt+esc sends a
//	          literal one; without an exit that is a single unshifted key, the
//	          operator is back to a keyboard takeover, which is the thing being
//	          removed.
//	alt+1..9  switches mates in place. Claude Code has no alt+digit binding, so
//	          reserving that namespace costs the mate nothing and means hopping
//	          between mates never requires leaving the keyboard.
func (m *Model) onTypeKey(msg tea.KeyMsg) tea.Cmd {
	if msg.Type == tea.KeyEsc && !msg.Alt {
		m.mode = modeNavigate
		m.logKey(msg, nil, false)
		return nil
	}
	if n, ok := altDigit(msg); ok {
		m.logKey(msg, nil, false)
		return m.selectIndex(n - 1)
	}
	if msg.Type == tea.KeyEsc && msg.Alt {
		// alt+esc is the literal-escape hatch. Send a bare ESC, not ESC ESC: the
		// alt modifier is the bridge's own addressing, not something the mate asked
		// for.
		t := m.current()
		if !m.canType(t) {
			m.logKey(msg, nil, false)
			return nil
		}
		data := []byte{0x1b}
		m.logKey(msg, data, true)
		return m.inputCmd(t, data)
	}

	t := m.current()
	if !m.canType(t) {
		// Type mode with nowhere to type: the pane says so (it shows "connecting"
		// or "exited", never a stale snapshot that looks live), so dropping the key
		// is honest rather than silent.
		m.logKey(msg, nil, false)
		return nil
	}
	data := EncodeKey(msg, t.screen.Mode(1), t.screen.Mode(2004))
	if len(data) == 0 {
		// A key with no terminal representation. It must send NOTHING — not a zero
		// byte, which is what used to print "^@" in the mate's prompt.
		m.logKey(msg, nil, false)
		return nil
	}
	m.logKey(msg, data, true)
	return m.inputCmd(t, data)
}

// canType reports whether a tab can receive keystrokes right now.
func (m *Model) canType(t *tab) bool {
	return t != nil && t.attached && t.screen != nil
}

// altDigit recognises alt+1 … alt+9 across the shapes bubbletea reports it in.
// The console decoder on Windows and the ANSI parser on unix disagree about
// whether the digit lands in Runes or only in String(), so both are checked.
func altDigit(msg tea.KeyMsg) (int, bool) {
	if !msg.Alt || msg.Paste {
		return 0, false
	}
	if len(msg.Runes) == 1 && msg.Runes[0] >= '1' && msg.Runes[0] <= '9' {
		return int(msg.Runes[0] - '0'), true
	}
	s := msg.String()
	if len(s) == len("alt+1") && strings.HasPrefix(s, "alt+") && s[4] >= '1' && s[4] <= '9' {
		return int(s[4] - '0'), true
	}
	return 0, false
}

// onNavigateKey is the command path. Every binding is a BARE key.
func (m *Model) onNavigateKey(msg tea.KeyMsg) tea.Cmd {
	t := m.current()
	switch msg.String() {
	case "enter", "i":
		return m.enterType()
	case "right", "l", "tab", "n":
		return m.selectDelta(1)
	case "left", "h", "shift+tab", "p":
		return m.selectDelta(-1)
	case "a":
		if len(m.pendings) == 0 {
			m.setNotice("no approvals waiting")
			return nil
		}
		m.overlay = overlayApprovals
		m.approvalIdx = m.firstApprovalIdxFor(m.currentPersona())
		return nil
	case "r":
		// The retry offered when a claim was refused. Not "takeover" — the
		// operator is not being asked to win an argument, just to try again.
		if t == nil || !t.attached {
			return nil
		}
		return m.claimCmd(t.persona)
	case "x":
		// Drop this pane's stream and hand its lease back, leaving the mate
		// running. Selecting the tab again re-attaches it.
		return m.detach(t)
	case "f":
		return tea.Batch(m.fetchStatus(), m.fetchPending())
	case "?":
		m.overlay = overlayHelp
		return nil
	case "q", "ctrl+c":
		return m.quit()
	}
	// Bare 1-9. This is the binding the tab bar has always advertised; now it is
	// the binding that exists.
	if n, err := strconv.Atoi(msg.String()); err == nil && n >= 1 && n <= 9 {
		return m.selectIndex(n - 1)
	}
	if n, ok := altDigit(msg); ok {
		return m.selectIndex(n - 1)
	}
	return nil
}

// enterType focuses the selected mate for typing: it attaches if needed and
// claims the writer lease, both without the operator asking for either.
func (m *Model) enterType() tea.Cmd {
	t := m.current()
	if t == nil {
		m.setNotice("no mate to type into")
		return nil
	}
	if t.exited {
		m.setNotice(Chrome(t.persona, 40) + " has exited — nothing to type into")
		return nil
	}
	m.mode = modeType
	return m.focusCmds(t)
}

// focusCmds is what "the operator just landed on this mate" costs: an attach if
// the tab has no stream, plus — in Type mode only — a claim of the writer lease.
//
// The claim is the /takeover call. Naming it a takeover in the UI was ceremony
// left over from the web client, where several browser tabs could watch the same
// mate; the endpoint grants it unconditionally, so for a single operator it is
// simply "make my keystrokes count". It is gated on Type mode so that merely
// browsing the tab bar cannot yank the keyboard out from under a browser viewer —
// reading is not writing.
func (m *Model) focusCmds(t *tab) tea.Cmd {
	if t == nil {
		return nil
	}
	var cmds []tea.Cmd
	if !t.attached && !t.attaching && !t.exited && !t.gone {
		// Clear `tried` so an attach that failed earlier is retried now that the
		// operator has explicitly asked for this pane.
		t.tried = false
		cmds = append(cmds, m.beginAttach(t))
	}
	if m.mode == modeType && t.attached && !t.writer {
		cmds = append(cmds, m.claimCmd(t.persona))
	}
	return tea.Batch(cmds...)
}

// claimCmd claims the writer lease. Silent on success.
func (m *Model) claimCmd(persona string) tea.Cmd {
	api := m.api
	return func() tea.Msg {
		err := api.PTYTakeover(context.Background(), persona)
		return simpleResultMsg{Persona: persona, What: "claim", Err: err}
	}
}

func (m *Model) currentPersona() string {
	if t := m.current(); t != nil {
		return t.persona
	}
	return ""
}

func (m *Model) selectDelta(d int) tea.Cmd {
	if len(m.tabs) == 0 {
		return nil
	}
	return m.selectIndex(((m.active+d)%len(m.tabs) + len(m.tabs)) % len(m.tabs))
}

// selectIndex switches tabs. Switching is a single assignment — no stream is
// stopped and no state is discarded, so background mates keep producing output
// into their own emulators and come back instantly.
//
// Selecting a mate ATTACHES it. There is no separate attach step and no "not
// attached" state the operator has to resolve; if we are in Type mode the new
// mate's keyboard is claimed too, so alt+2 is genuinely "now I am typing at
// mate 2".
//
// The mate we came from keeps its lease. Releasing on every hop would cost a
// round trip per keystroke-of-navigation and buys nothing: only the focused pane
// ever writes (see onFrame), and quit releases everything.
func (m *Model) selectIndex(i int) tea.Cmd {
	if i < 0 || i >= len(m.tabs) {
		return nil
	}
	m.active = i
	t := m.tabs[i]
	t.activity = false
	return m.focusCmds(t)
}

func (m *Model) quit() tea.Cmd {
	m.quitting = true
	// Hand every held keyboard back before leaving, the same thing the web
	// client does on pagehide. Without it, the next client finds every lock
	// held and has to wait out the two-minute lease.
	var cmds []tea.Cmd
	for _, t := range m.tabs {
		if t.attached {
			cmds = append(cmds, m.releaseCmd(t.persona))
		}
	}
	release := tea.Batch(cmds...)
	cancel := m.cancel
	return tea.Sequence(release, func() tea.Msg {
		cancel()
		return nil
	}, tea.Quit)
}

// Close cancels every stream and closes the keylog. Safe to call more than once.
func (m *Model) Close() {
	m.cancel()
	m.keylog.Close()
}

// --- mid-turn terminal confirmation -----------------------------------------
//
// This is the one prompt that survived the redesign, and it is NOT an attach step.
// The operator is never asked "do you want to attach?"; they are asked to consent
// to a destructive server-side action: ensurePTY KILLS a mid-turn headless process
// and resumes the session under a PTY, losing the in-flight answer. That is a data
// loss confirmation and it stays.

func (m *Model) onAskKey(msg tea.KeyMsg) tea.Cmd {
	t := m.tab(m.askPersona)
	switch msg.String() {
	case "o", "a", "enter":
		m.overlay = overlayNone
		if t == nil {
			return nil
		}
		t.attaching = true
		return m.startAndPrime(t.persona)
	case "w":
		m.overlay = overlayNone
		if t == nil {
			return nil
		}
		t.awaitIdle = true
		m.setNotice("will attach to " + Chrome(t.persona, 40) + " once its turn finishes")
		return nil
	case "esc", "q", "ctrl+c":
		m.overlay = overlayNone
		if t != nil {
			t.attaching = false
		}
		return nil
	}
	return nil
}

// --- approvals --------------------------------------------------------------

func (m *Model) firstApprovalIdxFor(persona string) int {
	for i, p := range m.pendings {
		if p.Persona == persona {
			return i
		}
	}
	return 0
}

func (m *Model) onApprovalKey(msg tea.KeyMsg) tea.Cmd {
	if len(m.pendings) == 0 {
		m.overlay = overlayNone
		return nil
	}
	switch msg.String() {
	case "esc", "q", "ctrl+c":
		m.overlay = overlayNone
		return nil
	case "j", "down":
		m.approvalIdx = min(m.approvalIdx+1, len(m.pendings)-1)
		return nil
	case "k", "up":
		m.approvalIdx = max(m.approvalIdx-1, 0)
		return nil
	case "g":
		// Jump to the blocked mate's pane, which is usually where the operator
		// wants to look before deciding.
		p := m.pendings[m.approvalIdx]
		m.overlay = overlayNone
		for i, t := range m.tabs {
			if t.persona == p.Persona {
				return m.selectIndex(i)
			}
		}
		return nil
	case "a", "enter":
		return m.resolveAt(m.approvalIdx, "allow", "")
	case "A":
		return m.resolveAt(m.approvalIdx, "allow", timeBoxDuration)
	case "d":
		return m.resolveAt(m.approvalIdx, "deny", "")
	case "D":
		return m.resolveAllFor(m.pendings[m.approvalIdx].Persona, "deny")
	}
	return nil
}

func (m *Model) resolveAt(i int, behavior, duration string) tea.Cmd {
	if i < 0 || i >= len(m.pendings) {
		return nil
	}
	p := m.pendings[i]
	// Drop the row locally so a second keypress can't double-resolve while the
	// request is in flight.
	m.pendings = append(m.pendings[:i:i], m.pendings[i+1:]...)
	m.rebuildByPersona()
	if m.approvalIdx >= len(m.pendings) {
		m.approvalIdx = max(0, len(m.pendings)-1)
	}
	if len(m.pendings) == 0 {
		m.overlay = overlayNone
	}
	api := m.api
	return func() tea.Msg {
		err := api.Resolve(context.Background(), p.ID, behavior, duration)
		return resolveResultMsg{ID: p.ID, Behavior: behavior, Err: err}
	}
}

func (m *Model) resolveAllFor(persona, behavior string) tea.Cmd {
	ids := make([]string, 0, 4)
	keep := m.pendings[:0:0]
	for _, p := range m.pendings {
		if p.Persona == persona {
			ids = append(ids, p.ID)
			continue
		}
		keep = append(keep, p)
	}
	m.pendings = keep
	m.rebuildByPersona()
	m.approvalIdx = 0
	if len(m.pendings) == 0 {
		m.overlay = overlayNone
	}
	api := m.api
	return func() tea.Msg {
		var firstErr error
		for _, id := range ids {
			if err := api.Resolve(context.Background(), id, behavior, ""); err != nil && firstErr == nil {
				firstErr = err
			}
		}
		return resolveResultMsg{ID: persona, Behavior: behavior, Err: firstErr}
	}
}

func (m *Model) rebuildByPersona() {
	m.byPersona = map[string][]Pending{}
	for _, p := range m.pendings {
		m.byPersona[p.Persona] = append(m.byPersona[p.Persona], p)
	}
}

// --- diagnostics for tests --------------------------------------------------

// tabState is a read-only snapshot used by tests to assert on internals without
// exporting the mutable tab type.
type tabState struct {
	Persona  string
	Status   string
	Attached bool
	Exited   bool
	Locked   bool
	Writer   bool
	Activity bool
	Pending  int
	Err      string
}

func (m *Model) tabStates() []tabState {
	out := make([]tabState, 0, len(m.tabs))
	for _, t := range m.tabs {
		out = append(out, tabState{
			Persona:  t.persona,
			Status:   t.status,
			Attached: t.attached,
			Exited:   t.exited,
			Locked:   t.locked,
			Writer:   t.writer,
			Activity: t.activity,
			Pending:  len(m.byPersona[t.persona]),
			Err:      t.err,
		})
	}
	return out
}
