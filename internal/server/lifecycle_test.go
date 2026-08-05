package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// nopWriteCloser stands in for a live crew process's stdin pipe.
type nopWriteCloser struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	closed   bool
	closeErr error
}

func (n *nopWriteCloser) Write(p []byte) (int, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.buf.Write(p)
}

func (n *nopWriteCloser) Close() error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.closed = true
	return n.closeErr
}

func (n *nopWriteCloser) isClosed() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closed
}

// TestCloseLiveSurvivesUnstartedProcesses: closeLive runs on the shutdown path
// and on the fleeted idle-reap path. A liveProc whose cmd never started has a
// nil Process — dereferencing it would panic inside the shutdown goroutine,
// taking the captain down instead of stopping it.
func TestCloseLiveSurvivesUnstartedProcesses(t *testing.T) {
	s, _ := newTestServer(t)
	a := &nopWriteCloser{}
	b := &nopWriteCloser{closeErr: errors.New("already gone")}
	s.mu.Lock()
	s.live["backend"] = &liveProc{persona: "backend", cmd: &exec.Cmd{}, stdin: a}
	s.live["frontend"] = &liveProc{persona: "frontend", cmd: &exec.Cmd{}, stdin: b}
	s.mu.Unlock()

	s.closeLive() // must not panic, and must not stop at the first Close error

	if !a.isClosed() || !b.isClosed() {
		t.Fatalf("closeLive skipped a mate: a=%v b=%v", a.isClosed(), b.isClosed())
	}
}

func TestClosePTYsSurvivesUnstartedProcesses(t *testing.T) {
	s, _ := newTestServer(t)
	attachFakePTY(t, s, "backend")
	s.closePTYs() // cmd is nil on these; must not panic
}

// TestPumpRetiresTheMate drives the stream-json pump end to end over a pipe:
// assistant text, thinking, and the per-turn result stats must all land in the
// feed, and EOF must retire the mate and give back its ref-count.
func TestPumpRetiresTheMate(t *testing.T) {
	s, _ := newTestServer(t)
	s.mu.Lock()
	s.live["backend"] = &liveProc{persona: "backend", cmd: &exec.Cmd{}, stdin: &nopWriteCloser{}}
	s.refs = 1
	s.mu.Unlock()

	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.pump("backend", pr) }()

	lines := []string{
		`{"type":"assistant","message":{"content":[{"type":"text","text":"on it"}]}}`,
		`{"type":"result","total_cost_usd":0.25,"duration_ms":1500}`,
	}
	for _, l := range lines {
		if _, err := io.WriteString(pw, l+"\n"); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	_ = pw.Close()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("pump did not exit on EOF")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, still := s.live["backend"]; still {
		t.Error("pump must remove the mate from s.live when its stdout closes")
	}
	if !s.exited["backend"] {
		t.Error("pump must mark the mate done")
	}
	if s.refs != 0 {
		t.Errorf("refs = %d after the mate exited, want 0", s.refs)
	}
	var sawText, sawResult bool
	for _, e := range s.events {
		if e.Type == "assistant" && e.Text == "on it" {
			sawText = true
		}
		if e.Type == "result" {
			sawResult = true
			if e.CostUSD != 0.25 || e.DurationMS != 1500 {
				t.Errorf("result stats lost: %+v", e)
			}
		}
	}
	if !sawText || !sawResult {
		t.Fatalf("pump dropped events: %+v", s.events)
	}
}

// TestPumpIgnoresGarbage: a crew process that writes a non-JSON line (a stray
// log, a crash banner) must terminate the pump cleanly rather than spin.
func TestPumpIgnoresGarbage(t *testing.T) {
	s, _ := newTestServer(t)
	pr, pw := io.Pipe()
	done := make(chan struct{})
	go func() { defer close(done); s.pump("backend", pr) }()
	_, _ = io.WriteString(pw, "this is not json at all\n")
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		_ = pw.Close()
		t.Fatal("pump spun on undecodable output")
	}
	_ = pw.Close()
}

// TestConcurrentEventsAndStatus runs the writers (hooks posting events) against
// the readers (/status.json polling) the way the captain actually sees them.
// Any missing lock shows up here as a lost event or a map-concurrency crash.
func TestConcurrentEventsAndStatus(t *testing.T) {
	s, h := newTestServer(t)
	const writers, each = 16, 40

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers: poll the two aggregate endpoints continuously.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					do(t, h, "GET", "/status.json", "")
					do(t, h, "GET", "/events", "")
					s.computeStatus(time.Now())
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				s.addEvent(Event{Persona: "mate", Type: "hook:PostToolUse", Text: "x"})
			}
		}(w)
	}

	// Wait for the writers, then stop the readers.
	go func() {
		time.Sleep(400 * time.Millisecond)
		close(stop)
	}()
	wg.Wait()

	s.mu.Lock()
	n := len(s.events)
	s.mu.Unlock()
	if n != writers*each {
		t.Fatalf("recorded %d events, want %d — an append was lost", n, writers*each)
	}
}

// TestConcurrentPendingRegistration checks the permission map under load: many
// mates hitting the gate at once must each get their own pending id, and
// /pending.json must never observe a torn map.
func TestConcurrentPendingRegistration(t *testing.T) {
	s, h := newTestServer(t)
	const n = 32

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.awaitDecision("mate", "Bash", "sleep 1")
		}()
	}

	// Wait for all of them to register.
	deadline := time.Now().Add(15 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.pendings)
		s.mu.Unlock()
		if got == n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d pendings registered — ids collided or a lock was dropped", got, n)
		}
		do(t, h, "GET", "/pending.json", "") // concurrent reader
		time.Sleep(2 * time.Millisecond)
	}

	// Every id must be unique and resolvable exactly once.
	s.mu.Lock()
	ids := make([]string, 0, len(s.pendings))
	chans := make([]chan string, 0, len(s.pendings))
	for id, p := range s.pendings {
		ids = append(ids, id)
		chans = append(chans, p.ch)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			t.Fatalf("duplicate pending id %q", ids[i])
		}
	}

	var out []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(do(t, h, "GET", "/pending.json", "").Body.Bytes(), &out); err != nil {
		t.Fatalf("pending.json under load: %v", err)
	}

	for _, ch := range chans {
		ch <- "deny"
	}
	wg.Wait()

	s.mu.Lock()
	left := len(s.pendings)
	s.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d pendings leaked after every one was resolved", left)
	}
}

func TestCrewPersonas(t *testing.T) {
	t.Chdir(t.TempDir())
	if got := crewPersonas(); got != nil {
		t.Fatalf("no .claude/agents should yield nil, got %v", got)
	}

	writePersona(t, "backend", "name: backend")
	writePersona(t, "security", "name: security")
	// Opted out of fleet membership — must not appear in the roster.
	writePersona(t, "local-helper", "shipmatesPersona: false")
	// Non-markdown files and directories are ignored.
	if err := os.WriteFile(filepath.Join(".claude/agents", "README.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".claude/agents", "nested.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := crewPersonas()
	sort.Strings(got)
	want := []string{"backend", "security"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("crewPersonas() = %v, want %v", got, want)
	}
}

// TestStatusShowsInstalledButUnrunCrewAsOff: the fleet roster should list every
// installed mate, not just the ones with process history, or you cannot address
// a mate that hasn't spoken yet.
func TestStatusShowsInstalledCrewAsOff(t *testing.T) {
	s, h := newTestServer(t)
	writePersona(t, "backend", "name: backend")
	writePersona(t, "quiet", "name: quiet")
	s.mu.Lock()
	s.live["backend"] = &liveProc{persona: "backend"}
	s.lastSeen["backend"] = time.Now()
	s.mu.Unlock()

	var out []MateStatus
	if err := json.Unmarshal(do(t, h, "GET", "/status.json", "").Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, st := range out {
		got[st.Persona] = st.Status
	}
	if got["backend"] != "working" {
		t.Errorf("backend = %q, want working", got["backend"])
	}
	if got["quiet"] != "off" {
		t.Errorf("an installed but unrun mate = %q, want off", got["quiet"])
	}
	if got["quiet"] != "off" || out[0].Persona != "backend" {
		t.Errorf("roster must be sorted by persona, got %+v", out)
	}
}

// TestAttachFilenameCannotEscapeTheInbox: the client controls the filename, so
// a traversal attempt must be discarded entirely — the server names the file
// itself from a UUID and only borrows the extension.
func TestAttachFilenameCannotEscapeTheInbox(t *testing.T) {
	for _, name := range []string{
		`../../../../evil.png`,
		`..\..\evil.png`,
		`C:\Windows\Temp\evil.png`,
		`/etc/cron.d/evil.png`,
		"evil\x00.png",
	} {
		t.Run(name, func(t *testing.T) {
			_, root, ts := newAttachTestServer(t)
			body, ctype := buildAttachRequest(t, name, "image/png", pngBytes, "")
			req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
			req.Header.Set("Content-Type", ctype)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				// Rejecting outright is also a fine outcome; what is NOT fine
				// is landing a file outside the inbox.
				return
			}
			var out AttachResponse
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(out.Path, ".shipmates/inbox/") || strings.Contains(out.Path, "..") {
				t.Fatalf("path escaped the inbox: %q", out.Path)
			}
			// The landed file must be a direct child of the inbox.
			entries, err := os.ReadDir(filepath.Join(root, ".shipmates", "inbox"))
			if err != nil {
				t.Fatalf("inbox unreadable: %v", err)
			}
			if len(entries) != 1 {
				t.Fatalf("inbox holds %d entries, want exactly the one upload", len(entries))
			}
			if entries[0].IsDir() {
				t.Fatal("upload created a directory inside the inbox")
			}
		})
	}
}

func TestAttachIDsAreDistinct(t *testing.T) {
	// Filenames come from a UUID prefix; a collision would silently drop an
	// upload (O_EXCL turns it into a 500) or overwrite one.
	_, root, ts := newAttachTestServer(t)
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		body, ctype := buildAttachRequest(t, "shot.png", "image/png", pngBytes, "")
		req, _ := http.NewRequest("POST", ts.URL+"/attach", body)
		req.Header.Set("Content-Type", ctype)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out AttachResponse
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("upload %d = %d", i, resp.StatusCode)
		}
		if seen[out.AttachID] {
			t.Fatalf("duplicate attach id %q", out.AttachID)
		}
		seen[out.AttachID] = true
	}
	entries, err := os.ReadDir(filepath.Join(root, ".shipmates", "inbox"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 25 {
		t.Fatalf("inbox holds %d files, want 25 — uploads collided", len(entries))
	}
}

func TestAttachInboxDirFallsBackToCWD(t *testing.T) {
	// projectRoot is captured at New(); a zero value (hand-built Server) must
	// still resolve somewhere sane rather than writing to the filesystem root.
	t.Chdir(t.TempDir())
	s := &Server{}
	dir := s.attachInboxDir()
	if !filepath.IsAbs(dir) {
		t.Fatalf("attachInboxDir() = %q, want an absolute path", dir)
	}
	if !strings.HasSuffix(filepath.ToSlash(dir), ".shipmates/inbox") {
		t.Fatalf("attachInboxDir() = %q", dir)
	}
}

// TestSweeperIgnoresDirectories guards against the sweeper trying (and
// failing, hourly) to os.Remove a subdirectory someone dropped in the inbox.
func TestSweeperIgnoresDirectories(t *testing.T) {
	root := t.TempDir()
	s := &Server{projectRoot: root}
	inbox := s.attachInboxDir()
	sub := filepath.Join(inbox, "keepme")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(sub, old, old); err != nil {
		t.Fatal(err)
	}
	s.attachSweepOnce(time.Now())
	if _, err := os.Stat(sub); err != nil {
		t.Fatalf("sweeper removed a directory: %v", err)
	}
}
