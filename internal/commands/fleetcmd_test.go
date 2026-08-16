package commands

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"
)

// isolateFleetEnv removes the ambient fleet credentials/URL so a developer (or
// CI) with these exported doesn't change what the tests exercise.
//
// These must be *unset*, not set to "": urfave/cli treats a set-but-empty
// environment source as a real value that beats the flag's default, so
// SHIPMATES_FLEET_URL="" would leave the base URL empty rather than
// http://127.0.0.1:8443.
func isolateFleetEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"SHIPMATES_FLEET_TOKEN", "SHIPMATES_FLEET_URL"} {
		if old, ok := os.LookupEnv(key); ok {
			t.Cleanup(func() { _ = os.Setenv(key, old) })
		}
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
}

// runWithOperatorFlags builds a throwaway command carrying operatorFlags() and
// runs fn inside its Action, so the flag/env resolution under test is exactly
// the one the real fleet subcommands get.
func runWithOperatorFlags(t *testing.T, args []string, fn func(ctx context.Context, c *cli.Command) error) error {
	t.Helper()
	cmd := &cli.Command{
		Name:  "probe",
		Flags: operatorFlags(),
		Action: func(ctx context.Context, c *cli.Command) error {
			return fn(ctx, c)
		},
	}
	return cmd.Run(context.Background(), append([]string{"probe"}, args...))
}

// ---------------------------------------------------------------------------
// loadFleetToken — the secret must never come from a CLI flag, and the file
// must win over the environment.
// ---------------------------------------------------------------------------

func TestLoadFleetToken(t *testing.T) {
	t.Run("empty when neither file nor env is set", func(t *testing.T) {
		isolateFleetEnv(t)
		got, err := loadFleetToken("")
		if err != nil {
			t.Fatal(err)
		}
		if got != "" {
			t.Errorf("token = %q, want empty (no auth)", got)
		}
	})

	t.Run("falls back to the environment", func(t *testing.T) {
		isolateFleetEnv(t)
		t.Setenv("SHIPMATES_FLEET_TOKEN", "  env-secret\n")
		got, err := loadFleetToken("")
		if err != nil {
			t.Fatal(err)
		}
		if got != "env-secret" {
			t.Errorf("token = %q, want the trimmed env value", got)
		}
	})

	t.Run("--token-file wins over the environment", func(t *testing.T) {
		isolateFleetEnv(t)
		t.Setenv("SHIPMATES_FLEET_TOKEN", "env-secret")
		dir := t.TempDir()
		path := filepath.Join(dir, "token")
		// Trailing newline is what `echo secret > token` produces.
		if err := os.WriteFile(path, []byte("file-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := loadFleetToken(path)
		if err != nil {
			t.Fatal(err)
		}
		if got != "file-secret" {
			t.Errorf("token = %q, want file-secret", got)
		}
	})

	t.Run("a whitespace-only --token-file value falls through to env", func(t *testing.T) {
		isolateFleetEnv(t)
		t.Setenv("SHIPMATES_FLEET_TOKEN", "env-secret")
		got, err := loadFleetToken("   ")
		if err != nil {
			t.Fatal(err)
		}
		if got != "env-secret" {
			t.Errorf("token = %q, want env-secret", got)
		}
	})

	t.Run("an unreadable token file is a clear error", func(t *testing.T) {
		isolateFleetEnv(t)
		_, err := loadFleetToken(filepath.Join(t.TempDir(), "nope"))
		if err == nil {
			t.Fatal("expected an error for a missing token file")
		}
		if !strings.Contains(err.Error(), "read token file") {
			t.Errorf("err = %v, want a 'read token file' error", err)
		}
	})
}

// ---------------------------------------------------------------------------
// fleetDo / fleetGet / fleetPost
// ---------------------------------------------------------------------------

func TestFleetDo_SendsBearerAndDecodesBody(t *testing.T) {
	isolateFleetEnv(t)
	t.Setenv("SHIPMATES_FLEET_TOKEN", "s3cret")

	var gotAuth, gotMethod, gotPath, gotCT string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotMethod, gotPath = r.Method, r.URL.Path
		gotCT = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		out, err := fleetPost(ctx, c, "/api/captain/x/tell/geordi", []byte(`{"message":"hi"}`))
		if err != nil {
			return err
		}
		if string(out) != `{"ok":true}` {
			t.Errorf("body = %q", out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q, want Bearer s3cret", gotAuth)
	}
	if gotMethod != http.MethodPost || gotPath != "/api/captain/x/tell/geordi" {
		t.Errorf("got %s %s", gotMethod, gotPath)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotCT)
	}
	if string(gotBody) != `{"message":"hi"}` {
		t.Errorf("body = %q", gotBody)
	}
}

// With no token configured we must not send an empty `Authorization: Bearer `
// header — a fleet started without auth would still see a malformed header.
func TestFleetDo_NoTokenSendsNoAuthHeader(t *testing.T) {
	isolateFleetEnv(t)
	sawAuth := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, sawAuth = r.Header["Authorization"]
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		_, err := fleetGet(ctx, c, "/api/captains")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if sawAuth {
		t.Error("an Authorization header was sent with no token configured")
	}
}

// GET carries no body and no Content-Type.
func TestFleetDo_GetHasNoBody(t *testing.T) {
	isolateFleetEnv(t)
	var gotCT string
	var gotLen int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		gotLen = r.ContentLength
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		_, err := fleetGet(ctx, c, "/api/captains")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "" {
		t.Errorf("GET sent Content-Type %q", gotCT)
	}
	if gotLen > 0 {
		t.Errorf("GET sent a %d-byte body", gotLen)
	}
}

// A 401 must read as an auth problem, with the remedy in the message.
func TestFleetDo_UnauthorizedIsExplained(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte("nope"))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		_, err := fleetGet(ctx, c, "/api/captains")
		return err
	})
	if err == nil {
		t.Fatal("expected an error on 401")
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Errorf("err = %v, want it to say unauthorized", err)
	}
	if !strings.Contains(err.Error(), "SHIPMATES_FLEET_TOKEN") && !strings.Contains(err.Error(), "--token-file") {
		t.Errorf("err = %v, want it to name the remedy", err)
	}
}

// Other non-2xx statuses surface the status code and the server's message.
func TestFleetDo_ErrorStatusIncludesServerMessage(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("  no such captain\n"))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		_, err := fleetGet(ctx, c, "/api/captain/ghost/feed")
		return err
	})
	if err == nil {
		t.Fatal("expected an error on 404")
	}
	if !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "no such captain") {
		t.Errorf("err = %v, want the status and the server message", err)
	}
}

// A --fleet value with a trailing slash must not produce a double slash.
func TestFleetDo_TrimsTrailingSlashOnBaseURL(t *testing.T) {
	isolateFleetEnv(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL + "/"}, func(ctx context.Context, c *cli.Command) error {
		_, err := fleetGet(ctx, c, "/api/captains")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/captains" {
		t.Errorf("path = %q, want /api/captains", gotPath)
	}
}

// The fleet URL is configurable by environment as well as by flag.
func TestOperatorFlags_FleetURLFromEnv(t *testing.T) {
	isolateFleetEnv(t)
	t.Setenv("SHIPMATES_FLEET_URL", "http://example.invalid:9999")
	err := runWithOperatorFlags(t, nil, func(ctx context.Context, c *cli.Command) error {
		if got := c.String("fleet"); got != "http://example.invalid:9999" {
			t.Errorf("fleet = %q, want the env value", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestOperatorFlags_DefaultFleetURL(t *testing.T) {
	isolateFleetEnv(t)
	err := runWithOperatorFlags(t, nil, func(ctx context.Context, c *cli.Command) error {
		if got := c.String("fleet"); got != "http://127.0.0.1:8443" {
			t.Errorf("default fleet URL = %q", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A broken token file must abort the request rather than silently going out
// unauthenticated.
func TestFleetDo_TokenFileErrorAbortsTheRequest(t *testing.T) {
	isolateFleetEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	missing := filepath.Join(t.TempDir(), "nope")
	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL, "--token-file", missing},
		func(ctx context.Context, c *cli.Command) error {
			_, err := fleetGet(ctx, c, "/api/captains")
			return err
		})
	if err == nil {
		t.Fatal("expected an error for an unreadable token file")
	}
	if called {
		t.Error("the request went out despite the token file failing to load")
	}
}

// ---------------------------------------------------------------------------
// fleetDoRaw / fleet show — multipart upload
// ---------------------------------------------------------------------------

func TestFleetDoRaw_SetsContentTypeAndBearer(t *testing.T) {
	isolateFleetEnv(t)
	t.Setenv("SHIPMATES_FLEET_TOKEN", "s3cret")

	var gotCT, gotAuth string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT, gotAuth = r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("stored"))
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		out, err := fleetDoRaw(ctx, c, "POST", "/api/captain/x/attach", "multipart/form-data; boundary=abc", []byte("payload"))
		if err != nil {
			return err
		}
		if string(out) != "stored" {
			t.Errorf("response = %q", out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotCT != "multipart/form-data; boundary=abc" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if gotAuth != "Bearer s3cret" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if string(gotBody) != "payload" {
		t.Errorf("body = %q", gotBody)
	}
}

func TestFleetDoRaw_UnauthorizedIsExplained(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := runWithOperatorFlags(t, []string{"--fleet", srv.URL}, func(ctx context.Context, c *cli.Command) error {
		_, err := fleetDoRaw(ctx, c, "POST", "/api/captain/x/attach", "text/plain", []byte("x"))
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("err = %v, want an unauthorized error", err)
	}
}

// `fleet show` uploads the file under the "file" form field, named by its
// basename, with the caption alongside.
func TestFleetShow_UploadsMultipart(t *testing.T) {
	isolateFleetEnv(t)

	var gotFilename, gotContent, gotCaption string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("bad content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(part)
			switch part.FormName() {
			case "file":
				gotFilename, gotContent = part.FileName(), string(b)
			case "caption":
				gotCaption = string(b)
			}
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "screenshot.png")
	if err := os.WriteFile(path, []byte("PNGDATA"), 0o644); err != nil {
		t.Fatal(err)
	}

	var err error
	_ = captureStdout(t, func() {
		err = Fleet().Run(context.Background(),
			[]string{"fleet", "show", "--fleet", srv.URL, "--caption", "look at this", "cap-1", path})
	})
	if err != nil {
		t.Fatalf("fleet show: %v", err)
	}
	if gotFilename != "screenshot.png" {
		t.Errorf("filename = %q, want the basename", gotFilename)
	}
	if gotContent != "PNGDATA" {
		t.Errorf("content = %q", gotContent)
	}
	if gotCaption != "look at this" {
		t.Errorf("caption = %q", gotCaption)
	}
}

func TestFleetShow_MissingFileIsAClearError(t *testing.T) {
	isolateFleetEnv(t)
	err := Fleet().Run(context.Background(),
		[]string{"fleet", "show", "cap-1", filepath.Join(t.TempDir(), "nope.png")})
	if err == nil {
		t.Fatal("expected an error for a missing file")
	}
	if !strings.Contains(err.Error(), "open ") {
		t.Errorf("err = %v, want an 'open <path>' error", err)
	}
}

// ---------------------------------------------------------------------------
// Operator subcommand argument validation — these must fail before any
// network call.
// ---------------------------------------------------------------------------

func TestFleetSubcommands_ArgValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"tail without a captain", []string{"fleet", "tail"}, "usage:"},
		{"pending without a captain", []string{"fleet", "pending"}, "usage:"},
		{"tell without enough args", []string{"fleet", "tell", "cap-1", "geordi"}, "usage:"},
		{"show without a file", []string{"fleet", "show", "cap-1"}, "usage:"},
		{"resolve without enough args", []string{"fleet", "resolve", "cap-1", "id-1"}, "usage:"},
		{"dispatch without enough args", []string{"fleet", "dispatch", "cap-1", "bd-1", "cap-2"}, "usage:"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateFleetEnv(t)
			err := Fleet().Run(context.Background(), tc.args)
			if err == nil {
				t.Fatalf("args %v: expected an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// `fleet resolve` only accepts allow|deny, and rejects anything else before
// touching the network.
func TestFleetResolve_BehaviorValidation(t *testing.T) {
	isolateFleetEnv(t)
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	err := Fleet().Run(context.Background(),
		[]string{"fleet", "resolve", "--fleet", srv.URL, "cap-1", "id-1", "maybe"})
	if err == nil {
		t.Fatal("expected an error for an invalid behavior")
	}
	if !strings.Contains(err.Error(), "allow|deny") {
		t.Errorf("err = %v, want it to name the allowed values", err)
	}
	if called {
		t.Error("an invalid behavior was sent to the fleet anyway")
	}
}

func TestFleetResolve_SendsBehavior(t *testing.T) {
	isolateFleetEnv(t)
	var gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	for _, behavior := range []string{"allow", "deny"} {
		if err := Fleet().Run(context.Background(),
			[]string{"fleet", "resolve", "--fleet", srv.URL, "cap-1", "id-1", behavior}); err != nil {
			t.Fatalf("resolve %s: %v", behavior, err)
		}
		if gotPath != "/api/captain/cap-1/resolve/id-1" {
			t.Errorf("path = %q", gotPath)
		}
		if !strings.Contains(string(gotBody), `"behavior":"`+behavior+`"`) {
			t.Errorf("body = %q, want behavior %s", gotBody, behavior)
		}
	}
}

// `fleet tell` joins the trailing words into one message rather than dropping
// everything after the first.
func TestFleetTell_JoinsTheMessage(t *testing.T) {
	isolateFleetEnv(t)
	var gotBody []byte
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()

	if err := Fleet().Run(context.Background(),
		[]string{"fleet", "tell", "--fleet", srv.URL, "cap-1", "geordi", "please", "check", "the", "build"}); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/captain/cap-1/tell/geordi" {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(string(gotBody), "please check the build") {
		t.Errorf("body = %q, want the whole message", gotBody)
	}
}

// `fleet beads` targets the fleet-wide graph with no argument and one ship's
// graph with a captain key.
func TestFleetBeads_PathSelection(t *testing.T) {
	isolateFleetEnv(t)
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	var err error
	_ = captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "beads", "--fleet", srv.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/beads" {
		t.Errorf("fleet-wide path = %q, want /api/beads", gotPath)
	}

	_ = captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "beads", "--fleet", srv.URL, "cap-1"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/captain/cap-1/beads" {
		t.Errorf("per-ship path = %q", gotPath)
	}
}

// A malformed JSON body from the fleet must be a decode error naming what
// failed, not a panic or a silent empty listing.
func TestFleetLs_MalformedJSON(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	err := Fleet().Run(context.Background(), []string{"fleet", "ls", "--fleet", srv.URL})
	if err == nil {
		t.Fatal("expected a decode error")
	}
	if !strings.Contains(err.Error(), "decode captains") {
		t.Errorf("err = %v, want a 'decode captains' error", err)
	}
}

func TestFleetLs_EmptyListIsFriendly(t *testing.T) {
	isolateFleetEnv(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	var err error
	out := captureStdout(t, func() {
		err = Fleet().Run(context.Background(), []string{"fleet", "ls", "--fleet", srv.URL})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "(no captains connected)") {
		t.Errorf("output = %q", out)
	}
}

// The subcommand tree is part of the CLI's contract — a rename would silently
// break operators' muscle memory and scripts.
func TestFleet_SubcommandSet(t *testing.T) {
	want := map[string]bool{
		"serve": true, "ls": true, "tail": true, "tell": true, "show": true,
		"pending": true, "resolve": true, "status": true, "beads": true, "dispatch": true,
	}
	got := map[string]bool{}
	for _, sub := range Fleet().Commands {
		got[sub.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("fleet subcommand %q is missing", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected fleet subcommand %q (update this test if it's intentional)", name)
		}
	}
}

// Every operator subcommand must accept --fleet and --token-file; a subcommand
// that forgot operatorFlags() would ignore the operator's target and secret.
func TestFleet_OperatorSubcommandsCarryTheSharedFlags(t *testing.T) {
	for _, sub := range Fleet().Commands {
		if sub.Name == "serve" { // serve has its own flag set
			continue
		}
		names := map[string]bool{}
		for _, f := range sub.Flags {
			for _, n := range f.Names() {
				names[n] = true
			}
		}
		if !names["fleet"] {
			t.Errorf("fleet %s is missing the --fleet flag", sub.Name)
		}
		if !names["token-file"] {
			t.Errorf("fleet %s is missing the --token-file flag", sub.Name)
		}
	}
}

// The shared secret must never be settable from the command line — that puts
// it in ps/cmdline for every user on the host.
func TestFleet_NoTokenFlagAnywhere(t *testing.T) {
	check := func(name string, flags []cli.Flag) {
		for _, f := range flags {
			for _, n := range f.Names() {
				if n == "token" {
					t.Errorf("fleet %s exposes a --token flag; secrets must come from a file or the environment", name)
				}
			}
		}
	}
	root := Fleet()
	check(root.Name, root.Flags)
	for _, sub := range root.Commands {
		check(sub.Name, sub.Flags)
	}
}

// ---------------------------------------------------------------------------
// L3 — the fleet URL must not put the bearer token on the wire in cleartext.
// ---------------------------------------------------------------------------

func TestFleetBaseURL_PlaintextRule(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr string // substring; empty means accepted
	}{
		{name: "https is the normal case", url: "https://fleet.example.com:8443"},
		{name: "https with a trailing slash", url: "https://fleet.example.com/"},
		{name: "loopback http is local development", url: "http://127.0.0.1:8443"},
		{name: "localhost http is local development", url: "http://localhost:8443"},
		{name: "ipv6 loopback http", url: "http://[::1]:8443"},

		{name: "plaintext to a dns host is refused", url: "http://fleet.example.com", wantErr: "plaintext"},
		{name: "plaintext to a LAN host is refused", url: "http://10.0.0.5:8443", wantErr: "plaintext"},
		{name: "a lookalike loopback host is refused", url: "http://127.0.0.1.evil.com:8443", wantErr: "plaintext"},
		{name: "credentials in the url are refused", url: "https://u:p@fleet.example.com", wantErr: "credentials"},
		{name: "a scheme-less url is refused", url: "fleet.example.com:8443", wantErr: "scheme"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			isolateFleetEnv(t)
			err := runWithOperatorFlags(t, []string{"--fleet", tc.url}, func(ctx context.Context, c *cli.Command) error {
				got, err := fleetBaseURL(c)
				if err != nil {
					return err
				}
				if strings.HasSuffix(got, "/") {
					t.Errorf("base %q keeps a trailing slash", got)
				}
				return nil
			})
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("--fleet %s: %v, want it accepted", tc.url, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("--fleet %s was accepted, want a refusal mentioning %q", tc.url, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), "--fleet") {
				t.Errorf("err = %v, want it to name the flag the operator has to change", err)
			}
		})
	}
}

// The URL is checked BEFORE the token is loaded and before any request goes
// out: a leaked token cannot be un-leaked by a later error.
func TestFleetDo_PlaintextURLIsRefusedBeforeTheTokenIsTouched(t *testing.T) {
	isolateFleetEnv(t)
	t.Setenv("SHIPMATES_FLEET_TOKEN", "s3cret")

	err := runWithOperatorFlags(t, []string{"--fleet", "http://fleet.example.com"},
		func(ctx context.Context, c *cli.Command) error {
			_, err := fleetGet(ctx, c, "/api/captains")
			return err
		})
	if err == nil {
		t.Fatal("a plaintext fleet URL was accepted")
	}
	if !strings.Contains(err.Error(), "plaintext") {
		t.Errorf("err = %v, want it to name the plaintext transport", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("err = %v leaks the token", err)
	}

	// With a broken token file AND a bad URL, the URL error must win — which
	// is only possible if the URL is validated first.
	err = runWithOperatorFlags(t,
		[]string{"--fleet", "http://fleet.example.com", "--token-file", filepath.Join(t.TempDir(), "nope")},
		func(ctx context.Context, c *cli.Command) error {
			_, err := fleetGet(ctx, c, "/api/captains")
			return err
		})
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("err = %v, want the URL to be refused before the credential is read", err)
	}
}

// The same rule applies to the multipart path (`fleet show`).
func TestFleetDoRaw_RefusesAPlaintextURL(t *testing.T) {
	isolateFleetEnv(t)
	err := runWithOperatorFlags(t, []string{"--fleet", "http://fleet.example.com"},
		func(ctx context.Context, c *cli.Command) error {
			_, err := fleetDoRaw(ctx, c, "POST", "/api/captain/x/attach", "text/plain", []byte("x"))
			return err
		})
	if err == nil || !strings.Contains(err.Error(), "plaintext") {
		t.Fatalf("err = %v, want a plaintext refusal", err)
	}
}

// The shipped default stays usable: a fleet on the operator's own machine.
func TestOperatorFlags_DefaultFleetURLIsAcceptedByTheTransportRule(t *testing.T) {
	isolateFleetEnv(t)
	err := runWithOperatorFlags(t, nil, func(ctx context.Context, c *cli.Command) error {
		base, err := fleetBaseURL(c)
		if err != nil {
			return err
		}
		if base != "http://127.0.0.1:8443" {
			t.Errorf("base = %q", base)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the default fleet URL was refused: %v", err)
	}
}
