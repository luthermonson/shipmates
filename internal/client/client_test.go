package client

import (
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

// chdirProject moves the test into a scratch dir that stands in for a project
// root. Every client function resolves the port file relative to cwd (via
// project.PortFile), so this is what isolates one test's server metadata from
// another's. t.Chdir restores the old cwd at test end.
func chdirProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Chdir(dir)
	return dir
}

// writePort records a port in the project's server.port file, the way the
// captain server does at boot.
func writePort(t *testing.T, contents string) {
	t.Helper()
	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.PortFile(), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

// testToken is the credential writeToken publishes — the stand-in for what a
// real captain mints from crypto/rand at startup.
const testToken = "0123456789abcdef0123456789abcdef"

// writeToken records an API token the way the captain server does at boot.
// Without it every helper here fails closed: the server authenticates.
func writeToken(t *testing.T, tok string) {
	t.Helper()
	if err := os.MkdirAll(project.SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.TokenFile(), []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// serveAt starts an httptest server and points the project's port file and
// token file at it, returning the server so the test can inspect what it
// received.
func serveAt(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	writeToken(t, testToken)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	_, portStr, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split %s: %v", srv.URL, err)
	}
	writePort(t, portStr+"\n")
	return srv
}

// deadPort returns a port number nothing is listening on: bind :0, note the
// port, then release it.
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

func TestPortReadsAndTrims(t *testing.T) {
	chdirProject(t)
	writePort(t, "  54321\r\n")
	got, err := port()
	if err != nil {
		t.Fatalf("port: %v", err)
	}
	if got != 54321 {
		t.Fatalf("port = %d, want 54321", got)
	}
}

func TestPortMissingFile(t *testing.T) {
	chdirProject(t)
	if _, err := port(); err == nil {
		t.Fatal("want error when the port file does not exist")
	}
}

func TestPortGarbageContents(t *testing.T) {
	for _, contents := range []string{"", "not-a-port", "80 80", "12.5", "0x50"} {
		t.Run(strconv.Quote(contents), func(t *testing.T) {
			chdirProject(t)
			writePort(t, contents)
			got, err := port()
			if err == nil {
				t.Fatalf("want error for %q, got port %d", contents, got)
			}
		})
	}
}

func TestBaseBuildsLoopbackURL(t *testing.T) {
	chdirProject(t)
	writePort(t, "8123")
	got, err := base()
	if err != nil {
		t.Fatalf("base: %v", err)
	}
	// Loopback literal, not "localhost": the server binds 127.0.0.1 only, and
	// on a dual-stack host "localhost" can resolve to ::1 first and fail.
	if got != "http://127.0.0.1:8123" {
		t.Fatalf("base = %q, want %q", got, "http://127.0.0.1:8123")
	}
}

func TestBasePropagatesPortError(t *testing.T) {
	chdirProject(t)
	writePort(t, "garbage")
	got, err := base()
	if err == nil {
		t.Fatalf("want error, got base %q", got)
	}
	if got != "" {
		t.Fatalf("base = %q on error, want empty", got)
	}
}

func TestHealthyNoPortFile(t *testing.T) {
	chdirProject(t)
	if Healthy() {
		t.Fatal("Healthy() = true with no port file")
	}
}

func TestHealthyUnreachableServer(t *testing.T) {
	chdirProject(t)
	writePort(t, strconv.Itoa(deadPort(t)))
	if Healthy() {
		t.Fatal("Healthy() = true when nothing is listening")
	}
}

func TestHealthyOK(t *testing.T) {
	chdirProject(t)
	var hits []string
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	if !Healthy() {
		t.Fatal("Healthy() = false against a 200 server")
	}
	if len(hits) != 1 || hits[0] != "/health" {
		t.Fatalf("server saw %v, want one probe of /health", hits)
	}
}

func TestHealthyNon200(t *testing.T) {
	chdirProject(t)
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	if Healthy() {
		t.Fatal("Healthy() = true for a 503 /health")
	}
}

// blockSpawn makes the spawn path fail before it can fork anything:
// EnsureRunning does os.Create(project.LogFile()) immediately before
// exec.Command, so occupying that path with a directory turns "would have
// spawned a detached server" into a plain error. Tests must never leave a real
// server process behind, so this is the guard rather than an after-the-fact
// assertion.
func blockSpawn(t *testing.T) {
	t.Helper()
	if err := os.MkdirAll(project.LogFile(), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestEnsureRunningSkipsSpawnWhenHealthy(t *testing.T) {
	chdirProject(t)
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	blockSpawn(t)
	// With the spawn path booby-trapped, a nil return can only mean the healthy
	// short-circuit fired.
	if err := EnsureRunning(); err != nil {
		t.Fatalf("EnsureRunning: %v — it tried to spawn despite a healthy server", err)
	}
}

func TestEnsureRunningReportsSetupFailure(t *testing.T) {
	chdirProject(t)
	writePort(t, strconv.Itoa(deadPort(t))) // unhealthy: spawn path is taken
	blockSpawn(t)
	if err := EnsureRunning(); err == nil {
		t.Fatal("want an error when the server log cannot be created")
	}
}

func TestGetReturnsBody(t *testing.T) {
	chdirProject(t)
	var gotPath, gotMethod string
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotMethod = r.URL.Path, r.Method
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	body, err := Get("/status")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(body) != `{"ok":true}` {
		t.Fatalf("body = %q", body)
	}
	if gotPath != "/status" || gotMethod != http.MethodGet {
		t.Fatalf("server saw %s %s, want GET /status", gotMethod, gotPath)
	}
}

func TestGetNoServer(t *testing.T) {
	chdirProject(t)
	if _, err := Get("/status"); err == nil {
		t.Fatal("want error with no port file")
	}
	writePort(t, strconv.Itoa(deadPort(t)))
	if _, err := Get("/status"); err == nil {
		t.Fatal("want error when nothing is listening")
	}
}

func TestPostSendsJSON(t *testing.T) {
	chdirProject(t)
	var gotPath, gotType string
	var gotBody map[string]any
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotType = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, "accepted")
	}))
	out, err := Post("/tell", map[string]any{"persona": "captain", "text": "ahoy"})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if string(out) != "accepted" {
		t.Fatalf("out = %q", out)
	}
	if gotPath != "/tell" {
		t.Fatalf("path = %q, want /tell", gotPath)
	}
	if gotType != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotType)
	}
	if gotBody["persona"] != "captain" || gotBody["text"] != "ahoy" {
		t.Fatalf("server decoded %v", gotBody)
	}
}

func TestPostNilBodySendsEmpty(t *testing.T) {
	chdirProject(t)
	var raw []byte
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	if _, err := Post("/shutdown", nil); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if len(raw) != 0 {
		t.Fatalf("nil body sent %q, want nothing", raw)
	}
}

func TestPostErrorStatusReturnsBodyAndError(t *testing.T) {
	chdirProject(t)
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = io.WriteString(w, "  no such persona\n")
	}))
	out, err := Post("/tell", map[string]string{"persona": "ghost"})
	if err == nil {
		t.Fatal("want error for a 418 response")
	}
	// The body is returned alongside the error so callers can show the server's
	// own message; the error text carries the trimmed body.
	if string(out) != "  no such persona\n" {
		t.Fatalf("body = %q, want the raw server body", out)
	}
	if !strings.Contains(err.Error(), "418") || !strings.Contains(err.Error(), "no such persona") {
		t.Fatalf("error = %q, want status and trimmed body", err)
	}
	if strings.Contains(err.Error(), "persona\n") {
		t.Fatalf("error = %q, body should be trimmed", err)
	}
}

func TestPostUnencodableBody(t *testing.T) {
	chdirProject(t)
	writePort(t, "8123")
	if _, err := Post("/tell", make(chan int)); err == nil {
		t.Fatal("want error for a body json cannot encode")
	}
}

func TestPostNoPortFile(t *testing.T) {
	chdirProject(t)
	if _, err := Post("/tell", nil); err == nil {
		t.Fatal("want error with no port file")
	}
}

func TestPostAttachUploadsMultipart(t *testing.T) {
	chdirProject(t)
	src := filepath.Join(t.TempDir(), "diagram.png")
	if err := os.WriteFile(src, []byte("\x89PNG fake bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotName, gotCaption, gotContent, gotPath string
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f, hdr, err := r.FormFile("file")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer f.Close()
		gotName = hdr.Filename
		b, _ := io.ReadAll(f)
		gotContent = string(b)
		gotCaption = r.FormValue("caption")
		_ = json.NewEncoder(w).Encode(AttachResp{AttachID: "att_1", Path: "/store/att_1.png", Size: int64(len(b))})
	}))

	resp, err := PostAttach(src, "look at this")
	if err != nil {
		t.Fatalf("PostAttach: %v", err)
	}
	if gotPath != "/attach" {
		t.Fatalf("path = %q, want /attach", gotPath)
	}
	// The form file must carry only the base name — the server stores by it,
	// and leaking the client's absolute path would be wrong on every OS.
	if gotName != "diagram.png" {
		t.Fatalf("filename = %q, want diagram.png", gotName)
	}
	if gotContent != "\x89PNG fake bytes" {
		t.Fatalf("uploaded %q", gotContent)
	}
	if gotCaption != "look at this" {
		t.Fatalf("caption = %q", gotCaption)
	}
	if resp.AttachID != "att_1" || resp.Path != "/store/att_1.png" || resp.Size != 15 {
		t.Fatalf("resp = %+v", resp)
	}
}

func TestPostAttachOmitsEmptyCaption(t *testing.T) {
	chdirProject(t)
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	var hadCaption bool
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		_, hadCaption = r.MultipartForm.Value["caption"]
		_, _ = io.WriteString(w, `{"attachId":"a"}`)
	}))
	if _, err := PostAttach(src, ""); err != nil {
		t.Fatalf("PostAttach: %v", err)
	}
	if hadCaption {
		t.Fatal("empty caption should not be sent as a form field")
	}
}

func TestPostAttachMissingFile(t *testing.T) {
	chdirProject(t)
	writePort(t, "8123")
	missing := filepath.Join(t.TempDir(), "gone.png")
	_, err := PostAttach(missing, "")
	if err == nil {
		t.Fatal("want error for a missing file")
	}
	if !strings.Contains(err.Error(), "open ") || !strings.Contains(err.Error(), "gone.png") {
		t.Fatalf("error = %q, want it to name the file it could not open", err)
	}
}

func TestPostAttachNoPortFile(t *testing.T) {
	chdirProject(t)
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := PostAttach(src, ""); err == nil {
		t.Fatal("want error with no port file")
	}
}

func TestPostAttachServerError(t *testing.T) {
	chdirProject(t)
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = io.WriteString(w, "attachment too large\n")
	}))
	resp, err := PostAttach(src, "")
	if err == nil {
		t.Fatal("want error for a 413 response")
	}
	if resp != nil {
		t.Fatalf("resp = %+v on error, want nil", resp)
	}
	if !strings.Contains(err.Error(), "413") || !strings.Contains(err.Error(), "attachment too large") {
		t.Fatalf("error = %q, want status and server message", err)
	}
}

func TestPostAttachUnparseableResponse(t *testing.T) {
	chdirProject(t)
	src := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(src, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "not json at all")
	}))
	_, err := PostAttach(src, "")
	if err == nil {
		t.Fatal("want error for a non-JSON 200 response")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Fatalf("error = %q, want a parse-response error", err)
	}
}

// The multipart body must be a well-formed multipart/form-data payload with a
// boundary that matches the Content-Type header — this reads it back with the
// stdlib reader rather than trusting the writer.
func TestPostAttachBodyIsWellFormedMultipart(t *testing.T) {
	chdirProject(t)
	src := filepath.Join(t.TempDir(), "a.bin")
	if err := os.WriteFile(src, []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	var parts []string
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			parts = append(parts, p.FormName())
		}
		_, _ = io.WriteString(w, `{"attachId":"a"}`)
	}))
	if _, err := PostAttach(src, "cap"); err != nil {
		t.Fatalf("PostAttach: %v", err)
	}
	want := []string{"file", "caption"}
	if len(parts) != len(want) {
		t.Fatalf("parts = %v, want %v", parts, want)
	}
	for i := range want {
		if parts[i] != want[i] {
			t.Fatalf("parts = %v, want %v (order matters: file first)", parts, want)
		}
	}
}

// TestEveryRequestCarriesTheToken pins the credential onto all three request
// builders. The coordination server refuses anything but its health probe
// without one, so a helper that forgets it is a silently broken CLI command.
func TestEveryRequestCarriesTheToken(t *testing.T) {
	chdirProject(t)
	got := map[string]string{}
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Path] = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, `{"attachId":"a"}`)
	}))

	src := filepath.Join(t.TempDir(), "shot.png")
	if err := os.WriteFile(src, []byte("\x89PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("/status.json"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := Post("/tell/backend", map[string]string{"message": "ahoy"}); err != nil {
		t.Fatalf("Post: %v", err)
	}
	if _, err := PostAttach(src, ""); err != nil {
		t.Fatalf("PostAttach: %v", err)
	}

	want := "Bearer " + testToken
	for _, path := range []string{"/status.json", "/tell/backend", "/attach"} {
		if got[path] != want {
			t.Errorf("%s sent Authorization %q, want %q", path, got[path], want)
		}
	}
}

// TestRequestsFailWithoutATokenFile: no credential on disk means no captain is
// running (or one from before this existed). Failing here beats sending an
// unauthenticated request and reporting the 401 as a mysterious server error.
func TestRequestsFailWithoutATokenFile(t *testing.T) {
	chdirProject(t)
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	if err := os.Remove(project.TokenFile()); err != nil {
		t.Fatal(err)
	}
	if _, err := Get("/status.json"); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("Get with no token file = %v, want a token error", err)
	}
	if _, err := Post("/tell/backend", nil); err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("Post with no token file = %v, want a token error", err)
	}
}

// TestTokenTrimsAndRejectsEmpty: the file is written with a trailing newline,
// and an empty one is corruption rather than a valid credential.
func TestTokenTrimsAndRejectsEmpty(t *testing.T) {
	chdirProject(t)
	writeToken(t, "  "+testToken+"  ")
	got, err := Token()
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if got != testToken {
		t.Fatalf("Token = %q, want %q", got, testToken)
	}
	writeToken(t, "   ")
	if _, err := Token(); err == nil {
		t.Fatal("want an error for an empty token file")
	}
}

// TestHealthyNeedsNoToken: the liveness probe is the one open endpoint, and
// EnsureRunning polls it before the captain has published anything.
func TestHealthyNeedsNoToken(t *testing.T) {
	chdirProject(t)
	serveAt(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("health probe sent a credential: %q", r.Header.Get("Authorization"))
		}
		_, _ = io.WriteString(w, "ok")
	}))
	if err := os.Remove(project.TokenFile()); err != nil {
		t.Fatal(err)
	}
	if !Healthy() {
		t.Fatal("Healthy() = false with no token file; the probe must not need one")
	}
}
