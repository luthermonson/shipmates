package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

type liveRoundTrip func(*http.Request) (*http.Response, error)

func (f liveRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func response(status int, body string, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: status, Header: h, Body: io.NopCloser(strings.NewReader(body))}
}

func writeLiveCommandDiscovery(t *testing.T) {
	t.Helper()
	root, err := project.CanonicalRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	scope, err := project.ScopeID(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(project.ServerRecordFile()), 0o755); err != nil {
		t.Fatal(err)
	}
	record, _ := json.Marshal(map[string]any{"schema_version": 1, "project_root": root, "project_scope": scope, "address": "127.0.0.1:1", "pid": os.Getpid(), "control_token": strings.Repeat("t", 43)})
	if err := os.WriteFile(project.ServerRecordFile(), record, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLiveCommandsRejectStaleCrossProjectServerEndToEnd(t *testing.T) {
	m11InstallHostileRuntimeGuard(t)
	if runtime.GOOS == "windows" {
		t.Skip("test fixture uses Unix executable names")
	}
	repo, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	shipmates := filepath.Join(binDir, "shipmates")
	fakeCodex := filepath.Join(binDir, "codex")
	build := func(out, pkg string) {
		t.Helper()
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GOCACHE="+filepath.Join(t.TempDir(), "go-cache"))
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}
	build(shipmates, ".")
	build(fakeCodex, "./internal/commands/testdata/fakecodex")

	root := t.TempDir()
	projectA := filepath.Join(root, "project-a")
	projectB := filepath.Join(root, "project-b")
	for _, dir := range []string{projectA, projectB} {
		if err := os.MkdirAll(filepath.Join(dir, ".codex", "agents"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, ".shipmates", "sessions"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, ".shipmates", "policies"), 0o755); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{
			filepath.Join(dir, ".shipmates", "policy.yaml"),
			filepath.Join(dir, ".shipmates", "policies", "backend.yaml"),
		} {
			if err := os.WriteFile(path, []byte(emptyStrictPolicy), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, ".codex", "agents", "backend.toml"), []byte("developer_instructions = \"backend role\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "shipmates.yaml"), []byte("sessionPrefix: test\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := append(os.Environ(), "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	run := func(dir string, args ...string) ([]byte, error) {
		t.Helper()
		cmd := exec.Command(shipmates, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	cleanup := func(dir string) { _, _ = run(dir, "server", "stop") }
	defer cleanup(projectA)
	defer cleanup(projectB)

	if out, err := run(projectA, "live", "backend", "project A prompt"); err != nil {
		t.Fatalf("project A live: %v\n%s", err, out)
	}
	recordA, err := os.ReadFile(filepath.Join(projectA, ".shipmates", "sessions", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the smoke-test failure deterministically: fresh project B has a
	// stale discovery record pointing at project A's still-live server.
	if err := os.WriteFile(filepath.Join(projectB, ".shipmates", "sessions", "server.json"), recordA, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := run(projectB, "live", "backend", "project B prompt")
	if err != nil {
		t.Fatalf("project B live: %v\n%s", err, out)
	}
	line := bytes.SplitN(out, []byte("\n"), 2)[0]
	var snap struct {
		SessionID string `json:"session_id"`
		ThreadID  string `json:"thread_id"`
		TurnID    string `json:"turn_id"`
	}
	if json.Unmarshal(line, &snap) != nil || snap.SessionID == "" || snap.ThreadID != "thread-project-b" || snap.TurnID != "turn-1" {
		t.Fatalf("project B IDs not captured from live: %s", out)
	}
	feed, err := run(projectB, "feed", "backend")
	if err != nil {
		t.Fatalf("project B feed: %v\n%s", err, feed)
	}
	if !bytes.Contains(feed, []byte(`"session_id":"`+snap.SessionID+`"`)) || bytes.Contains(feed, []byte("thread-project-a")) {
		t.Fatalf("feed crossed project boundary: %s", feed)
	}
	tell, err := run(projectB, "tell", "backend", snap.SessionID, snap.ThreadID, snap.TurnID, "steer B")
	if err != nil || (!bytes.Contains(tell, []byte(`"code":""`)) && !bytes.Contains(tell, []byte("not_steerable"))) {
		t.Fatalf("tell did not steer or fail closed: %v\n%s", err, tell)
	}
	// Exercise exact-tuple interruption/terminal handling before shutdown.
	interrupt, interruptErr := run(projectB, "interrupt", "backend", snap.SessionID, snap.ThreadID, snap.TurnID)
	if interruptErr != nil && !bytes.Contains(interrupt, []byte("stale_target")) {
		t.Fatalf("interrupt exact target: %v\n%s", interruptErr, interrupt)
	}
}

func TestCodexLiveCommandPresentationAndExactTargets(t *testing.T) {
	t.Chdir(t.TempDir())
	writeLiveCommandDiscovery(t)
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	var paths []string
	http.DefaultTransport = liveRoundTrip(func(r *http.Request) (*http.Response, error) {
		paths = append(paths, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.URL.Path == "/health":
			return response(200, "ok", map[string]string{"X-Shipmates-Project": r.Header.Get("X-Shipmates-Project")}), nil
		case r.Method == "GET":
			return response(200, "{\"schema_version\":1,\"sequence\":9,\"persona\":\"backend\",\"session_id\":\"s1\",\"kind\":\"activity\",\"data\":{\"category\":\"command\"}}\n", map[string]string{"X-Shipmates-History-Dropped": "true"}), nil
		case strings.HasSuffix(r.URL.Path, "/tell"):
			return response(200, "{\"code\":\"\",\"target\":{\"persona\":\"backend\"}}\n", nil), nil
		case strings.HasSuffix(r.URL.Path, "/interrupt"):
			return response(200, "{\"code\":\"already_interrupting\",\"target\":{\"persona\":\"backend\"}}\n", nil), nil
		default:
			return response(200, "{\"persona\":\"backend\",\"session_id\":\"s1\",\"thread_id\":\"th1\",\"turn_id\":\"tu1\",\"state\":\"working\"}\n", nil), nil
		}
	})
	for _, tc := range []struct {
		cmd              *cli.Command
		args             []string
		wantOut, wantErr string
	}{{Live(), []string{"live", "backend", "secret prompt"}, `"session_id":"s1"`, "live session ready"}, {Feed(), []string{"feed", "backend", "--after", "1"}, `"kind":"activity"`, "history was dropped"}, {Tell(), []string{"tell", "backend", "s1", "th1", "tu1", "secret steer"}, `"target"`, ""}, {Interrupt(), []string{"interrupt", "backend", "s1", "th1", "tu1"}, `already_interrupting`, ""}} {
		var out, diag bytes.Buffer
		tc.cmd.Writer = &out
		tc.cmd.ErrWriter = &diag
		if err := tc.cmd.Run(context.Background(), tc.args); err != nil {
			t.Fatalf("%s: %v", tc.args[0], err)
		}
		if !strings.Contains(out.String(), tc.wantOut) || strings.Contains(out.String(), "secret") {
			t.Fatalf("%s stdout=%q", tc.args[0], out.String())
		}
		if !strings.Contains(diag.String(), tc.wantErr) || strings.Contains(diag.String(), "secret") {
			t.Fatalf("%s stderr=%q", tc.args[0], diag.String())
		}
	}
	joined := strings.Join(paths, "\n")
	for _, want := range []string{"POST /api/live/backend", "GET /api/live/backend/feed?after=1&follow=false", "POST /api/live/backend/tell", "POST /api/live/backend/interrupt"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %v", want, paths)
		}
	}
}

func TestCodexLiveCommandFailureHasNoSuccessStdout(t *testing.T) {
	t.Chdir(t.TempDir())
	writeLiveCommandDiscovery(t)
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	http.DefaultTransport = liveRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/health" {
			return response(200, "ok", map[string]string{"X-Shipmates-Project": r.Header.Get("X-Shipmates-Project")}), nil
		}
		return response(409, "{\"code\":\"stale_target\",\"message\":\"redacted\"}", nil), nil
	})
	var out bytes.Buffer
	cmd := Tell()
	cmd.Writer = &out
	err := cmd.Run(context.Background(), []string{"tell", "backend", "old", "thread", "turn", "secret"})
	if err == nil || err.Error() != "stale_target" || out.Len() != 0 {
		t.Fatalf("err=%v stdout=%q", err, out.String())
	}
}

func TestInterruptCommandRejectsMissingAndEmptyTurnID(t *testing.T) {
	for _, args := range [][]string{
		{"interrupt", "backend", "session", "thread"},
		{"interrupt", "backend", "session", "thread", ""},
	} {
		cmd := Interrupt()
		var out bytes.Buffer
		cmd.Writer = &out
		err := cmd.Run(context.Background(), args)
		if err == nil || err.Error() != "invalid_target" || out.Len() != 0 {
			t.Fatalf("args=%q err=%v stdout=%q", args, err, out.String())
		}
	}
}

func TestInterruptCommandPreservesServerTargetErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	writeLiveCommandDiscovery(t)
	old := http.DefaultTransport
	defer func() { http.DefaultTransport = old }()
	for _, code := range []string{"stale_target", "invalid_target"} {
		t.Run(code, func(t *testing.T) {
			http.DefaultTransport = liveRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/health" {
					return response(200, "ok", map[string]string{"X-Shipmates-Project": r.Header.Get("X-Shipmates-Project")}), nil
				}
				return response(409, `{"code":"`+code+`"}`, nil), nil
			})
			cmd := Interrupt()
			var out bytes.Buffer
			cmd.Writer = &out
			err := cmd.Run(context.Background(), []string{"interrupt", "backend", "session", "thread", "turn"})
			if err == nil || err.Error() != code || out.Len() != 0 {
				t.Fatalf("code=%s err=%v stdout=%q", code, err, out.String())
			}
		})
	}
}
