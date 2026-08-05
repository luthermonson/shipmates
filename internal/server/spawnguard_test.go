package server

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// These tests cover the guards that run BEFORE any child process is started.
// The spawn paths themselves need a real `claude` on PATH and are left to
// integration testing; what is testable — and worth testing — is that a
// misconfigured mate is refused cleanly instead of hanging a request or
// leaking a half-registered process entry.

// TestEnsureLiveRefusesCommandBackedMates: a `backend: command` mate (opencode,
// aider, …) has no stream-json channel, so a tell can only reach it through a
// PTY. Without this guard the captain would spawn `claude` for a persona that
// is not backed by claude at all.
func TestEnsureLiveRefusesCommandBackedMates(t *testing.T) {
	s, h := newTestServer(t)
	writePersona(t, "opencode", "backend: command\ncommand: [\"opencode\", \"--tui\"]")

	lp, err := s.ensureLive("opencode")
	if err == nil {
		t.Fatal("want an error for a command-backed mate, got a live proc")
	}
	if lp != nil {
		t.Fatal("no liveProc may be returned on the refusal path")
	}
	if !strings.Contains(err.Error(), "PTY-only") {
		t.Fatalf("error should tell the operator what to do instead, got %q", err)
	}

	// Nothing may be registered for a mate that was never spawned.
	s.mu.Lock()
	n := len(s.live)
	s.mu.Unlock()
	if n != 0 {
		t.Fatalf("refused spawn registered %d live procs", n)
	}

	// And the same refusal must surface through /tell as a 500 with the
	// explanation, not a hang.
	w := do(t, h, "POST", "/tell/opencode", `{"message":"hi"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("tell to a command-backed mate = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), "PTY-only") {
		t.Fatalf("body = %q", w.Body.String())
	}
}

// TestPTYStartRejectsMisconfiguredCommandMates: `backend: command` with no
// command, or with a binary that isn't installed, must fail the request rather
// than leave a half-built ptyProc in the map for later requests to trip over.
func TestPTYStartRejectsMisconfiguredCommandMates(t *testing.T) {
	cases := []struct {
		name, frontmatter string
	}{
		{"no command", "backend: command"},
		{"binary not installed", "backend: command\ncommand: [\"shipmates-no-such-binary-xyz\"]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, h := newTestServer(t)
			writePersona(t, "broken", tc.frontmatter)

			w := do(t, h, "POST", "/pty/broken/start", "")
			if w.Code != http.StatusInternalServerError {
				t.Fatalf("= %d, want 500 (%s)", w.Code, w.Body.String())
			}
			s.mu.Lock()
			n := len(s.ptys)
			refs := s.refs
			s.mu.Unlock()
			if n != 0 {
				t.Fatalf("a failed start left %d ptys registered", n)
			}
			if refs != 0 {
				t.Fatalf("a failed start took a ref-count (%d); the captain would never idle out", refs)
			}
		})
	}
}

// TestBeadsSyncLoopReturnsWithoutAWorkspace: the loop is started
// unconditionally from Run, so on a non-beads project it must return
// immediately — not tick forever, and above all not shell out to bd.
func TestBeadsSyncLoopReturnsWithoutAWorkspace(t *testing.T) {
	t.Chdir(t.TempDir())
	s := New()
	done := make(chan struct{})
	go func() { defer close(done); s.beadsSyncLoop(context.Background()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("beadsSyncLoop did not bail out on a project with no .beads")
	}
}

func TestBeadsSyncLoopReturnsWithoutASyncRemote(t *testing.T) {
	// A local-only beads workspace has nothing to push to; syncing would just
	// fail every heartbeat.
	t.Chdir(t.TempDir())
	enableBeads(t)
	s := New()
	done := make(chan struct{})
	go func() { defer close(done); s.beadsSyncLoop(context.Background()) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("beadsSyncLoop did not bail out without a sync remote")
	}
}

// TestAttachSweeperLoopStopsOnShutdown: the sweeper sweeps once at startup
// (so a restart doesn't wait an hour) and must then exit on either the
// context or the server's stop channel — a loop that ignores stopCh keeps the
// process alive after /shutdown.
func TestAttachSweeperLoopStopsOnShutdown(t *testing.T) {
	for _, how := range []string{"context", "stopCh"} {
		t.Run(how, func(t *testing.T) {
			root := t.TempDir()
			s := New()
			s.projectRoot = root

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan struct{})
			go func() { defer close(done); s.attachSweeperLoop(ctx) }()

			if how == "context" {
				cancel()
			} else {
				s.stopOnce.Do(func() { close(s.stopCh) })
			}
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatalf("attachSweeperLoop ignored %s", how)
			}
		})
	}
}
