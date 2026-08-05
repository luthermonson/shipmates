package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// NOTE ON SCOPE: every test in this file deliberately stops short of runBD.
// `bd` may well be installed on the machine running these tests, and letting a
// handler shell out would spin up bd's embedded dolt server against a throwaway
// temp directory — slow, stateful, and nothing to do with the code under test.
// What is worth pinning is exactly the part that runs BEFORE the exec: the
// workspace gate and the argument validation that keeps caller-controlled text
// out of the bd command line.

// enableBeads creates a .beads directory in the current (sandboxed) cwd so the
// workspace gate opens.
func enableBeads(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(".beads", 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBeadsEnabled(t *testing.T) {
	t.Chdir(t.TempDir())
	if beadsEnabled() {
		t.Fatal("a bare directory is not a beads workspace")
	}
	// A FILE named .beads must not count as a workspace.
	if err := os.WriteFile(".beads", []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if beadsEnabled() {
		t.Fatal("a regular file named .beads must not enable beads")
	}
	if err := os.Remove(".beads"); err != nil {
		t.Fatal(err)
	}
	enableBeads(t)
	if !beadsEnabled() {
		t.Fatal("a .beads directory should enable beads")
	}
}

// TestBeadEndpointsGateOnWorkspace: without a workspace every bead route must
// 404 rather than shell out to a bd that has nothing to talk to.
func TestBeadEndpointsGateOnWorkspace(t *testing.T) {
	_, h := newTestServer(t) // sandbox has no .beads
	cases := []struct{ method, path, body string }{
		{"GET", "/beads.json", ""},
		{"GET", "/beads/summary", ""},
		{"GET", "/bead/proj-abc", ""},
		{"POST", "/bead", `{"title":"x"}`},
		{"POST", "/bead/proj-abc/close", `{}`},
		{"POST", "/bead/proj-abc/update", `{"priority":"1"}`},
		{"POST", "/beads/pull", ""},
	}
	for _, tc := range cases {
		t.Run(tc.method+tc.path, func(t *testing.T) {
			if w := do(t, h, tc.method, tc.path, tc.body); w.Code != http.StatusNotFound {
				t.Fatalf("= %d, want 404", w.Code)
			}
		})
	}
}

// TestBeadIDOK is the injection guard on every {id} path segment. bd is invoked
// with exec.Command (no shell), so this is defense in depth against argument
// smuggling — most importantly a leading dash, which argv WOULD read as a flag.
func TestBeadIDOK(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		{"proj-c03", true},
		{"proj-a3f8.1", true},
		{"a", true},
		{"UPPER-123", true},
		{"under_score", true},
		{"0", true},

		{"", false},
		{"-rf", false},
		{"--json", false},
		{"-", false},
		{"proj c03", false},
		{"proj/c03", false},
		{"proj\\c03", false},
		{"proj;rm -rf /", false},
		{"proj&whoami", false},
		{"proj|cat", false},
		{"$(whoami)", false},
		{"`whoami`", false},
		{"proj\nc03", false},
		{"proj\x00c03", false},
		{"../../etc/passwd", false},
		{"pröj-c03", false},
		{strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), false},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			if got := beadIDOK(tc.id); got != tc.want {
				t.Fatalf("beadIDOK(%q) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

// TestBeadIDRejectedBeforeExec proves the guard runs ahead of runBD: a bad id
// must come back 400 (validation), never 502 (bd said no).
func TestBeadIDRejectedBeforeExec(t *testing.T) {
	_, h := newTestServer(t)
	enableBeads(t)
	for _, id := range []string{"-rf", "bad%20id", "a$b"} {
		t.Run(id, func(t *testing.T) {
			for _, req := range []struct{ method, path, body string }{
				{"GET", "/bead/" + id, ""},
				{"POST", "/bead/" + id + "/close", `{}`},
				{"POST", "/bead/" + id + "/update", `{"priority":"1"}`},
			} {
				w := do(t, h, req.method, req.path, req.body)
				if w.Code != http.StatusBadRequest {
					t.Errorf("%s %s = %d, want 400 (a 502 would mean bd was invoked)", req.method, req.path, w.Code)
				}
			}
		})
	}
}

func TestBeadCreateValidation(t *testing.T) {
	_, h := newTestServer(t)
	enableBeads(t)
	cases := []struct {
		name, body string
		want       int
	}{
		{"no body", "", http.StatusBadRequest},
		{"malformed json", "{", http.StatusBadRequest},
		{"missing title", `{"description":"x"}`, http.StatusBadRequest},
		{"blank title", `{"title":"   "}`, http.StatusBadRequest},
		{"priority out of range", `{"title":"x","priority":"5"}`, http.StatusBadRequest},
		{"multi-char priority", `{"title":"x","priority":"10"}`, http.StatusBadRequest},
		{"non-numeric priority", `{"title":"x","priority":"p"}`, http.StatusBadRequest},
		{"unknown type", `{"title":"x","type":"wishlist"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(t, h, "POST", "/bead", tc.body); w.Code != tc.want {
				t.Fatalf("= %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestBeadUpdateValidation(t *testing.T) {
	_, h := newTestServer(t)
	enableBeads(t)
	cases := []struct {
		name, body string
		want       int
	}{
		{"malformed json", "{", http.StatusBadRequest},
		// Nothing to change: reject rather than invoke `bd update <id>` with
		// no flags, which would be a confusing no-op at the far end.
		{"no fields", `{}`, http.StatusBadRequest},
		{"blank fields only", `{"assignee":"  ","priority":"  ","title":"  "}`, http.StatusBadRequest},
		{"bad priority", `{"priority":"9"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if w := do(t, h, "POST", "/bead/proj-abc/update", tc.body); w.Code != tc.want {
				t.Fatalf("= %d, want %d (%s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

func TestBeadsSyncRemote(t *testing.T) {
	cases := []struct {
		name, config string
		want         bool
	}{
		{"no file", "", false},
		{"commented out", "# sync.remote: origin\n", false},
		{"empty value", "sync.remote:\n", false},
		{"blank value", "sync.remote:   \n", false},
		{"quoted empty", `sync.remote: ""` + "\n", false},
		{"plain value", "sync.remote: origin\n", true},
		{"quoted value", `sync.remote: "origin"` + "\n", true},
		{"single-quoted value", "sync.remote: 'origin'\n", true},
		{"indented among other keys", "db: x\n  sync.remote: origin\nother: y\n", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			if tc.config != "" {
				if err := os.MkdirAll(".beads", 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(".beads", "config.yaml"), []byte(tc.config), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := beadsSyncRemote(); got != tc.want {
				t.Fatalf("beadsSyncRemote() = %v, want %v (config %q)", got, tc.want, tc.config)
			}
		})
	}
}

// TestBeadsPullAsyncQueuesWithoutRunningBD: the fleet's fire-and-forget nudge
// must return 202 immediately and just wake the sync loop. Only ?wait=1 is
// allowed to block on an actual `bd dolt pull`, which this test never asks for.
func TestBeadsPullAsyncQueues(t *testing.T) {
	s, h := newTestServer(t)
	enableBeads(t)
	if err := os.WriteFile(filepath.Join(".beads", "config.yaml"), []byte("sync.remote: origin\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	w := do(t, h, "POST", "/beads/pull", "")
	if w.Code != http.StatusAccepted {
		t.Fatalf("= %d, want 202 (%s)", w.Code, w.Body.String())
	}
	select {
	case <-s.beadsTrigger:
	default:
		t.Fatal("an async pull must wake the sync loop")
	}
	// A pull is not a local write, so it must not mark the graph dirty —
	// otherwise every ship would re-announce every pull it received.
	s.beadsMu.Lock()
	dirty := s.beadsDirty
	s.beadsMu.Unlock()
	if dirty {
		t.Fatal("requestBeadsPull must not set the dirty flag")
	}
}

func TestBeadsPullRequiresSyncRemote(t *testing.T) {
	// A local-only beads workspace has nothing to pull from.
	_, h := newTestServer(t)
	enableBeads(t)
	if w := do(t, h, "POST", "/beads/pull", ""); w.Code != http.StatusNotFound {
		t.Fatalf("= %d, want 404", w.Code)
	}
}

func TestBeadsStateSigCombinesMultipleManifests(t *testing.T) {
	// A workspace can hold several dolt databases; the signature must fold in
	// all of them, or a change in the second one would go unannounced.
	t.Chdir(t.TempDir())
	write := func(db, content string) {
		p := filepath.Join(".beads", "embeddeddolt", db, ".dolt", "noms", "manifest")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("one", "hash-1")
	sigA := beadsStateSig()
	write("two", "hash-2")
	sigB := beadsStateSig()
	if sigA == sigB {
		t.Fatal("adding a second database must change the signature")
	}
	write("two", "hash-3")
	if sigC := beadsStateSig(); sigC == sigB {
		t.Fatal("a change in the second database must change the signature")
	}
}

func TestRequestBeadsPullCoalesces(t *testing.T) {
	s := New()
	for i := 0; i < 5; i++ {
		s.requestBeadsPull() // must never block on the cap-1 trigger
	}
	if len(s.beadsTrigger) != 1 {
		t.Fatalf("trigger depth = %d, want a single coalesced wake-up", len(s.beadsTrigger))
	}
}

func TestInvalidateBeadsSummary(t *testing.T) {
	// The badge cache must be dropped after a write so the operator's own
	// change is reflected immediately instead of up to 30s later.
	beadsSummaryMu.Lock()
	beadsSummaryOpen = 7
	beadsSummaryAt = time.Now()
	beadsSummaryMu.Unlock()

	invalidateBeadsSummary()

	beadsSummaryMu.Lock()
	defer beadsSummaryMu.Unlock()
	if !beadsSummaryAt.IsZero() {
		t.Fatal("invalidateBeadsSummary must zero the cache timestamp")
	}
}
