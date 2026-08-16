package ship

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// projectDir makes a scratch project dir with .shipmates/sessions/ in place,
// the layout every serverPort/serverHealthy/StatusAll probe reads from.
func projectDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".shipmates", "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeSession(t *testing.T, dir, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, ".shipmates", "sessions", name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// serveInProject starts an httptest server and records its port in dir, the
// way a live captain server would.
func serveInProject(t *testing.T, dir string, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	writeSession(t, dir, "server.port", portStr+"\n")
	writeSession(t, dir, "server.token", supervisorTestToken+"\n")
	return srv
}

// supervisorTestToken stands in for the captain's per-run API credential,
// which the supervisor must present to shut a captain down.
const supervisorTestToken = "0123456789abcdef0123456789abcdef"

// deadPort returns a port nothing listens on — the stale-port-file case.
func deadPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestServerPort(t *testing.T) {
	tests := []struct {
		name     string
		contents string // "" means: write no file at all
		write    bool
		want     int
	}{
		{name: "absent", want: 0},
		{name: "plain", contents: "5150", write: true, want: 5150},
		{name: "trailing newline", contents: "5150\n", write: true, want: 5150},
		{name: "crlf and spaces", contents: "  5150\r\n", write: true, want: 5150},
		{name: "garbage", contents: "not-a-port", write: true, want: 0},
		{name: "empty", contents: "", write: true, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := projectDir(t)
			if tt.write {
				writeSession(t, dir, "server.port", tt.contents)
			}
			if got := serverPort(dir); got != tt.want {
				t.Fatalf("serverPort = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestServerHealthyNoPortFile(t *testing.T) {
	if serverHealthy(projectDir(t)) {
		t.Fatal("serverHealthy = true with no port file")
	}
}

func TestServerHealthyStalePortFile(t *testing.T) {
	dir := projectDir(t)
	writeSession(t, dir, "server.port", strconv.Itoa(deadPort(t)))
	if serverHealthy(dir) {
		t.Fatal("serverHealthy = true for a stale port file — a crashed captain would never be restarted")
	}
}

func TestServerHealthyProbesHealthEndpoint(t *testing.T) {
	dir := projectDir(t)
	var paths []string
	serveInProject(t, dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	if !serverHealthy(dir) {
		t.Fatal("serverHealthy = false against a 200 server")
	}
	if len(paths) != 1 || paths[0] != "/health" {
		t.Fatalf("server saw %v, want one probe of /health", paths)
	}
}

func TestServerHealthyNon200(t *testing.T) {
	dir := projectDir(t)
	serveInProject(t, dir, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	if serverHealthy(dir) {
		t.Fatal("serverHealthy = true for a 500 /health")
	}
}

func TestShutdownServer(t *testing.T) {
	dir := projectDir(t)
	var gotPath, gotMethod, gotAuth string
	serveInProject(t, dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod, gotAuth = r.URL.Path, r.Method, r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	if !shutdownServer(dir) {
		t.Fatal("shutdownServer = false against an accepting server")
	}
	if gotMethod != http.MethodPost || gotPath != "/shutdown" {
		t.Fatalf("server saw %s %s, want POST /shutdown", gotMethod, gotPath)
	}
	// /shutdown authenticates like every other route: a supervisor that
	// forgets the token gets a 401 and silently stops reaping captains.
	if want := "Bearer " + supervisorTestToken; gotAuth != want {
		t.Fatalf("Authorization = %q, want %q", gotAuth, want)
	}
}

// TestShutdownServerWithoutToken: a captain whose token file is gone (or a
// captain from before tokens existed) answers 401, and the supervisor must
// report the failure so its hard-kill fallback fires.
func TestShutdownServerWithoutToken(t *testing.T) {
	dir := projectDir(t)
	serveInProject(t, dir, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+supervisorTestToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	if err := os.Remove(filepath.Join(dir, ".shipmates", "sessions", "server.token")); err != nil {
		t.Fatal(err)
	}
	if shutdownServer(dir) {
		t.Fatal("shutdownServer = true against a 401 — the hard-kill fallback would never fire")
	}
}

func TestShutdownServerRejected(t *testing.T) {
	dir := projectDir(t)
	serveInProject(t, dir, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	// A refusal must report false so the caller falls back to a hard kill.
	if shutdownServer(dir) {
		t.Fatal("shutdownServer = true for a 404 — the hard-kill fallback would never fire")
	}
}

func TestShutdownServerNoPortFile(t *testing.T) {
	if shutdownServer(projectDir(t)) {
		t.Fatal("shutdownServer = true with no port file")
	}
}

func TestSleepCtx(t *testing.T) {
	t.Run("completes", func(t *testing.T) {
		if !sleepCtx(context.Background(), time.Millisecond) {
			t.Fatal("sleepCtx = false for an uncancelled context")
		}
	})
	t.Run("cancelled first", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		start := time.Now()
		if sleepCtx(ctx, time.Hour) {
			t.Fatal("sleepCtx = true for a cancelled context")
		}
		if d := time.Since(start); d > 5*time.Second {
			t.Fatalf("sleepCtx waited %v after cancel, want an immediate return", d)
		}
	})
}

func TestSuperviseLoopReturnsOnCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// exe is deliberately bogus: a correct implementation checks ctx before
		// spawning anything, so this must never be executed.
		superviseLoop(ctx, "definitely-not-a-real-binary", Project{Dir: projectDir(t)}, nil)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("superviseLoop did not return for an already-cancelled context")
	}
}

func TestSuperviseLoopStandsByWhenCaptainAlreadyRunning(t *testing.T) {
	dir := projectDir(t)
	probes := make(chan struct{}, 16)
	serveInProject(t, dir, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case probes <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusOK)
	}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		// A bogus exe again: standing by must not spawn. If the loop tried, the
		// spawn would fail instantly and it would churn the backoff ladder
		// rather than sleep on the health probe.
		superviseLoop(ctx, "definitely-not-a-real-binary", Project{Dir: dir}, nil)
	}()

	select {
	case <-probes:
	case <-time.After(10 * time.Second):
		cancel()
		t.Fatal("superviseLoop never probed the already-running captain")
	}
	// It should now be parked on the healthProbe sleep, not restarting anything.
	select {
	case <-done:
		t.Fatal("superviseLoop exited instead of standing by")
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("superviseLoop did not return after cancel")
	}
}

func TestRunReturnsWhenContextAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &Config{Projects: []Project{{Dir: projectDir(t)}, {Dir: projectDir(t)}}}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, c) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return for an already-cancelled context")
	}
}

func TestRunWaitsForEverySupervisor(t *testing.T) {
	// Two projects with healthy captains: Run must stay blocked until the
	// context is cancelled, otherwise it would return while supervisors are
	// still live and the ship command would exit out from under them.
	dirs := []string{projectDir(t), projectDir(t)}
	for _, d := range dirs {
		serveInProject(t, d, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, &Config{Projects: []Project{{Dir: dirs[0]}, {Dir: dirs[1]}}}) }()

	select {
	case err := <-done:
		t.Fatalf("Run returned early (%v) while its supervisors were still standing by", err)
	case <-time.After(300 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestRunCaptainReportsStartFailureAndPreparesLog(t *testing.T) {
	dir := projectDir(t)
	logPath := filepath.Join(dir, ".shipmates", "sessions", "server.log")
	// A binary that cannot possibly exist: Start fails, nothing is spawned.
	err := runCaptain(context.Background(), filepath.Join(dir, "no-such-shipmates-binary"), dir, nil)
	if err == nil {
		t.Fatal("want an error when the captain binary cannot be started")
	}
	// The log is opened before Start, so it exists even on a failed spawn —
	// that's the file the supervisor tells operators to read.
	if _, statErr := os.Stat(logPath); statErr != nil {
		t.Fatalf("runCaptain did not prepare %s: %v", logPath, statErr)
	}
}

func TestStatusAll(t *testing.T) {
	live := projectDir(t)
	serveInProject(t, live, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	writeSession(t, live, "server.pid", "4242\n")

	stale := projectDir(t)
	writeSession(t, stale, "server.port", strconv.Itoa(deadPort(t)))

	bare := projectDir(t)

	c := &Config{Projects: []Project{{Dir: live}, {Dir: stale}, {Dir: bare}}}
	got := StatusAll(c)
	if len(got) != 3 {
		t.Fatalf("got %d statuses, want 3", len(got))
	}
	if !got[0].Running || got[0].PID != 4242 || got[0].Port == 0 || got[0].Dir != live {
		t.Fatalf("live project status = %+v", got[0])
	}
	if got[1].Running {
		t.Fatalf("stale project reported running: %+v", got[1])
	}
	if got[1].Port == 0 {
		t.Fatalf("stale project should still report its recorded port: %+v", got[1])
	}
	if got[1].PID != 0 {
		t.Fatalf("stale project has no pid file, got PID %d", got[1].PID)
	}
	if got[2].Running || got[2].Port != 0 || got[2].PID != 0 {
		t.Fatalf("bare project status = %+v, want all zero", got[2])
	}
}

func TestStatusAllEmptyConfig(t *testing.T) {
	got := StatusAll(&Config{})
	if got == nil {
		t.Fatal("StatusAll returned nil; callers range over it and encode it as JSON")
	}
	if len(got) != 0 {
		t.Fatalf("got %d statuses, want 0", len(got))
	}
}
