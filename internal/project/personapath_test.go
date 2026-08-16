package project

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// hostilePersonas are the shapes a persona name arrives in when it came from a
// URL wildcard rather than from a file on disk. ServeMux percent-decodes
// PathValue, so every one of these is reachable at the captain's HTTP surface.
var hostilePersonas = []struct{ name, why string }{
	{"../escape", "parent-directory hop"},
	{"..", "bare parent-directory hop"},
	{"../../../../etc/passwd", "deep hop"},
	{"nested/name", "path separator"},
	{"nested\\name", "windows path separator"},
	{"-rf", "leading dash reads as a flag"},
	{"--dangerously-skip-permissions", "leading dash reads as a flag"},
	{"/absolute", "absolute path"},
	{"C:/absolute", "windows absolute path"},
	{"Captain", "uppercase is not the rule"},
	{"with space", "whitespace"},
	{"with\nnewline", "request-line framing"},
	{"", "empty"},
}

// TestPersonaPathHelpersRefuseIllegalNames is the half of M1 that does not
// depend on a handler remembering to validate. Each helper must (a) keep the
// result inside the directory it advertises and (b) produce something the OS
// refuses to open, so a caller that skipped validation gets an error instead
// of a file somewhere else on the disk.
func TestPersonaPathHelpersRefuseIllegalNames(t *testing.T) {
	helpers := []struct {
		name   string
		fn     func(string) string
		parent string
	}{
		{"AgentPath", AgentPath, filepath.Clean(AgentsDir)},
		{"MemoryDir", MemoryDir, filepath.Join(Dir, MemoryDirName)},
		{"SessionMarker", SessionMarker, filepath.Join(Dir, SessionsDirName)},
		{"PolicyPath", PolicyPath, filepath.Join(Dir, PoliciesDirName)},
	}
	for _, h := range helpers {
		for _, tc := range hostilePersonas {
			got := h.fn(tc.name)

			// Containment: the result must still name a child of the
			// directory the helper is documented to live in. This is the
			// assertion that a traversal did not happen, independent of
			// whether anything then tried to open it.
			if filepath.Dir(got) != h.parent {
				t.Errorf("%s(%q) = %q, escaped %q (%s)", h.name, tc.name, got, h.parent, tc.why)
			}
			if strings.Contains(filepath.ToSlash(got), "..") {
				t.Errorf("%s(%q) = %q, still carries a parent hop (%s)", h.name, tc.name, got, tc.why)
			}

			// Unopenable: the refusal is enforced by the OS, not by a
			// convention. Reading, writing and stat'ing must all fail.
			if _, err := os.Stat(got); err == nil {
				t.Errorf("%s(%q) = %q, which stat'd successfully", h.name, tc.name, got)
			}
			if err := os.WriteFile(got, []byte("x"), 0o600); err == nil {
				t.Errorf("%s(%q) = %q, which was writable", h.name, tc.name, got)
				_ = os.Remove(got)
			}
			if _, err := os.ReadFile(got); err == nil {
				t.Errorf("%s(%q) = %q, which was readable", h.name, tc.name, got)
			}
		}
	}
}

// TestPersonaPathHelpersKeepLegalNames: the guard must not change the layout
// for every name that is actually in use.
func TestPersonaPathHelpersKeepLegalNames(t *testing.T) {
	cases := []struct{ got, want string }{
		{AgentPath("captain"), filepath.Join(AgentsDir, "captain.md")},
		{MemoryDir("first-mate"), filepath.Join(Dir, MemoryDirName, "first-mate")},
		{SessionMarker("backend_2"), filepath.Join(Dir, SessionsDirName, "backend_2.session")},
		{PolicyPath("security"), filepath.Join(Dir, PoliciesDirName, "security.yaml")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

// TestSessionMetaIORefusesIllegalNames: the write is the one that mattered
// most — a .session marker is a file created with JSON the caller influences.
// Nothing may be created anywhere for an illegal name.
func TestSessionMetaIORefusesIllegalNames(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "ship")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{sandbox, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(sandbox)

	for _, tc := range hostilePersonas {
		if err := WriteSessionMeta(tc.name, "n", "id", "fp", ""); err == nil {
			t.Errorf("WriteSessionMeta(%q) = nil, want a refusal (%s)", tc.name, tc.why)
		} else if !errors.Is(err, ErrInvalidPersona) {
			t.Errorf("WriteSessionMeta(%q) = %v, want ErrInvalidPersona", tc.name, err)
		}
		if _, ok := ReadSessionMeta(tc.name); ok {
			t.Errorf("ReadSessionMeta(%q) reported a session", tc.name)
		}
		if err := DeleteSessionMeta(tc.name); !errors.Is(err, ErrInvalidPersona) {
			t.Errorf("DeleteSessionMeta(%q) = %v, want ErrInvalidPersona", tc.name, err)
		}
		if _, err := ResolvePersonaConfig(tc.name); !errors.Is(err, ErrInvalidPersona) {
			t.Errorf("ResolvePersonaConfig(%q) = %v, want ErrInvalidPersona", tc.name, err)
		}
	}

	// Containment: nothing was created anywhere under the temp root, inside
	// the sandbox or out of it.
	var created []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && p != sandbox && p != outside {
			created = append(created, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("illegal persona names created files: %v", created)
	}
}

// TestPersonaPathHelpersCannotReachAFileOutsideTheCheckout is the arbitrary-
// read half of M1, made concrete: a real *.md sitting outside the checkout,
// and a persona name spelled to point at it. Pre-fix this resolved to that
// file and ResolvePersonaConfig parsed its frontmatter.
func TestPersonaPathHelpersCannotReachAFileOutsideTheCheckout(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "ship")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{sandbox, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	secret := filepath.Join(outside, "secret.md")
	if err := os.WriteFile(secret, []byte("---\nmodel: leaked\n---\ntop secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sandbox)

	// .claude/agents/<persona>.md — three hops reaches root/outside.
	const persona = "../../../outside/secret"
	if _, err := os.ReadFile(AgentPath(persona)); err == nil {
		t.Fatalf("AgentPath(%q) = %q, which read the file outside the checkout", persona, AgentPath(persona))
	}
	cfg, err := ResolvePersonaConfig(persona)
	if !errors.Is(err, ErrInvalidPersona) {
		t.Fatalf("ResolvePersonaConfig(%q) = %v, want ErrInvalidPersona", persona, err)
	}
	if cfg.Model == "leaked" {
		t.Fatal("frontmatter from outside the checkout was parsed")
	}
}

// TestSessionMarkerIsPrivate is L1/L2: the marker holds the session UUID that
// `claude --resume` treats as the whole identity of a conversation, so it is
// not world-readable.
func TestSessionMarkerIsPrivate(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := WriteSessionMeta("captain", "repo-captain", "uuid-1", "fp", ""); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}
	info, err := os.Stat(SessionMarker("captain"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS == "windows" {
		// Go synthesizes 0666 for any writable file on Windows; the mode
		// argument buys nothing there and access is decided by the inherited
		// DACL of .shipmates. Asserting 0600 would be asserting a fiction —
		// the position internal/recovery takes for its journal and PR #40 took
		// for the token file. What IS checkable is the other half of
		// WritePrivateFile's contract, exercised below.
		if info.IsDir() {
			t.Fatalf("%s is a directory", SessionMarker("captain"))
		}
		return
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("session marker mode = %04o, want 0600", perm)
	}
}

// TestSessionMarkerRewritesLoosePermissions: os.WriteFile does not re-apply
// the mode to a file that already exists, so a 0644 marker written by a
// pre-fix build would otherwise keep its mode forever. WritePrivateFile
// replaces the file instead of writing through it.
func TestSessionMarkerRewritesLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission bits are not meaningful on Windows; see TestSessionMarkerReplacesRatherThanWritesThrough")
	}
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := SessionMarker("captain")
	if err := os.WriteFile(stale, []byte(`{"name":"stale","id":"stale-uuid"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteSessionMeta("captain", "repo-captain", "uuid-2", "fp", ""); err != nil {
		t.Fatalf("WriteSessionMeta: %v", err)
	}
	meta, ok := ReadSessionMeta("captain")
	if !ok || meta.ID != "uuid-2" {
		t.Fatalf("marker was not replaced: %+v ok=%v", meta, ok)
	}
	info, err := os.Stat(stale)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("stale 0644 marker kept mode %04o, want 0600", perm)
	}
}

// TestSessionMarkerReplacesRatherThanWritesThrough is the half of
// WritePrivateFile's contract that IS observable on Windows, where the mode
// argument buys nothing and access is decided by the inherited DACL. Replacing
// rather than writing through is what stops a stale marker keeping whatever
// attributes a previous run left on it — and here it is the difference between
// succeeding and failing outright, because os.WriteFile cannot open a
// read-only file for writing while os.Remove clears the attribute first.
//
// Runs everywhere: the same replacement is what re-applies 0600 on unix.
func TestSessionMarkerReplacesRatherThanWritesThrough(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(SessionsDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := SessionMarker("captain")
	if err := os.WriteFile(stale, []byte(`{"name":"stale","id":"stale-uuid"}`), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stale, 0o600) })

	if err := WriteSessionMeta("captain", "repo-captain", "uuid-3", "fp", ""); err != nil {
		t.Fatalf("WriteSessionMeta over a read-only marker: %v", err)
	}
	meta, ok := ReadSessionMeta("captain")
	if !ok || meta.ID != "uuid-3" {
		t.Fatalf("marker was written through instead of replaced: %+v ok=%v", meta, ok)
	}
}
