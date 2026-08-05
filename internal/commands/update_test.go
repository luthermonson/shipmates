package commands

import (
	"bufio"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// everything it wrote. Several of the update/render helpers print directly to
// stdout; capturing keeps `go test -v` readable AND lets us assert on the
// operator-facing text (the conflict menu is a UI contract).
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()

	defer func() {
		os.Stdout = orig
		_ = w.Close()
	}()
	fn()
	os.Stdout = orig
	_ = w.Close()
	out := <-done
	_ = r.Close()
	return out
}

// newTestState builds an updateState reading its menu answers from lines.
func newTestState(interactive bool, lines string) *updateState {
	return &updateState{
		in:          bufio.NewScanner(strings.NewReader(lines)),
		interactive: interactive,
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// reconcileFile — the four-case update logic. This is the code that decides
// whether a user's hand-edited persona gets overwritten, so every branch is
// worth a test.
// ---------------------------------------------------------------------------

// A file the manifest never recorded and that isn't on disk is not ours to
// install during `update` — `add` installs, `update` only refreshes.
func TestReconcileFile_MissingAndUnrecordedIsNoOp(t *testing.T) {
	t.Chdir(t.TempDir())
	m := &project.Manifest{Files: map[string]string{}}
	st := newTestState(false, "")
	dst := filepath.Join(".claude", "agents", "geordi.md")

	if err := reconcileFile(m, st, dst, []byte("catalog\n"), "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("update installed a persona it was never asked to install (err=%v)", err)
	}
	if len(m.Files) != 0 {
		t.Errorf("manifest gained an entry: %v", m.Files)
	}
	if st.added+st.updated+st.kept+st.conflicts+st.skipped != 0 {
		t.Errorf("counters moved on a no-op: %+v", st)
	}
}

// A file shipmates installed that the user deleted gets re-added.
func TestReconcileFile_MissingButRecordedIsReAdded(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte("old\n"))}}
	st := newTestState(false, "")

	if err := reconcileFile(m, st, dst, []byte("catalog v2\n"), "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if got := readFile(t, dst); got != "catalog v2\n" {
		t.Errorf("re-added content = %q, want the catalog version", got)
	}
	if m.Files[dst] != project.SHA([]byte("catalog v2\n")) {
		t.Error("manifest baseline not advanced to the re-added content")
	}
	if st.added != 1 {
		t.Errorf("added = %d, want 1", st.added)
	}
}

// Content already identical to the catalog is a no-op, but an untracked file
// gets adopted into the manifest so a later catalog bump is a clean update
// rather than a conflict.
func TestReconcileFile_IdenticalAdoptsUntracked(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	body := "same bytes\n"
	mustWrite(t, dst, body)

	m := &project.Manifest{Files: map[string]string{}}
	st := newTestState(false, "")
	if err := reconcileFile(m, st, dst, []byte(body), "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if m.Files[dst] != project.SHA([]byte(body)) {
		t.Error("identical-but-untracked file was not adopted into the manifest")
	}
	if st.skipped != 1 {
		t.Errorf("skipped = %d, want 1", st.skipped)
	}
}

// Shipmates installed it and the user never touched it → safe to overwrite.
func TestReconcileFile_UnchangedByUserIsOverwritten(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	installed := "v1\n"
	mustWrite(t, dst, installed)

	m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte(installed))}}
	st := newTestState(false, "")
	if err := reconcileFile(m, st, dst, []byte("v2\n"), "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if got := readFile(t, dst); got != "v2\n" {
		t.Errorf("content = %q, want v2", got)
	}
	if st.updated != 1 {
		t.Errorf("updated = %d, want 1", st.updated)
	}
}

// The user edited it but the catalog hasn't moved → their edits stand, and it
// is explicitly NOT a conflict.
func TestReconcileFile_UserEditedCatalogUnchangedIsKept(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	catBytes := []byte("shipped v1\n")
	mustWrite(t, dst, "my own edits\n")

	m := &project.Manifest{Files: map[string]string{dst: project.SHA(catBytes)}}
	st := newTestState(false, "")
	if err := reconcileFile(m, st, dst, catBytes, "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if got := readFile(t, dst); got != "my own edits\n" {
		t.Errorf("user edits clobbered: %q", got)
	}
	if st.kept != 1 || st.conflicts != 0 {
		t.Errorf("want kept=1 conflicts=0, got kept=%d conflicts=%d", st.kept, st.conflicts)
	}
}

// The headline guarantee: both sides moved, nobody is at a terminal → keep the
// user's bytes, byte for byte, and don't advance the manifest baseline (so the
// conflict is re-offered next run instead of silently disappearing).
func TestReconcileFile_ConflictNonInteractiveKeepsUserBytes(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	mine := "MY PRECIOUS EDITS\n"
	baseline := "shipped v1\n"
	mustWrite(t, dst, mine)

	m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte(baseline))}}
	st := newTestState(false, "")
	if err := reconcileFile(m, st, dst, []byte("shipped v2\n"), "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if got := readFile(t, dst); got != mine {
		t.Fatalf("SILENT CLOBBER: file = %q, want %q", got, mine)
	}
	if m.Files[dst] != project.SHA([]byte(baseline)) {
		t.Error("baseline advanced on an unresolved conflict; the conflict would vanish next run")
	}
	if st.conflicts != 1 || st.kept != 1 {
		t.Errorf("want conflicts=1 kept=1, got conflicts=%d kept=%d", st.conflicts, st.kept)
	}
}

// An unrecorded pre-existing file that differs from the catalog is a conflict,
// not a free overwrite — shipmates didn't put it there.
func TestReconcileFile_UnrecordedDivergentFileIsAConflict(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	mine := "hand written persona\n"
	mustWrite(t, dst, mine)

	m := &project.Manifest{Files: map[string]string{}}
	st := newTestState(false, "")
	if err := reconcileFile(m, st, dst, []byte("catalog version\n"), "persona"); err != nil {
		t.Fatalf("reconcileFile: %v", err)
	}
	if got := readFile(t, dst); got != mine {
		t.Fatalf("pre-existing file clobbered: %q", got)
	}
	if st.conflicts != 1 {
		t.Errorf("conflicts = %d, want 1", st.conflicts)
	}
}

// --accept theirs / --accept ours reach reconcileFile as a sticky resolution.
func TestReconcileFile_StickyResolutions(t *testing.T) {
	catBytes := []byte("shipped v2\n")
	baseline := "shipped v1\n"
	mine := "mine\n"

	t.Run("sticky take overwrites and advances the baseline", func(t *testing.T) {
		t.Chdir(t.TempDir())
		dst := filepath.Join(".claude", "agents", "geordi.md")
		mustWrite(t, dst, mine)
		m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte(baseline))}}
		st := newTestState(false, "")
		st.stickyAll, st.stickyRes = true, resTake

		if err := reconcileFile(m, st, dst, catBytes, "persona"); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, dst); got != string(catBytes) {
			t.Errorf("content = %q, want the shipped version", got)
		}
		if m.Files[dst] != project.SHA(catBytes) {
			t.Error("baseline not advanced after taking theirs")
		}
		if st.updated != 1 {
			t.Errorf("updated = %d, want 1", st.updated)
		}
	})

	t.Run("sticky keep leaves the file alone", func(t *testing.T) {
		t.Chdir(t.TempDir())
		dst := filepath.Join(".claude", "agents", "geordi.md")
		mustWrite(t, dst, mine)
		m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte(baseline))}}
		st := newTestState(false, "")
		st.stickyAll, st.stickyRes = true, resKeep

		if err := reconcileFile(m, st, dst, catBytes, "persona"); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, dst); got != mine {
			t.Errorf("content = %q, want the user's version", got)
		}
		if st.kept != 1 {
			t.Errorf("kept = %d, want 1", st.kept)
		}
	})

	t.Run("sidecar writes <file>.new and never touches the original", func(t *testing.T) {
		t.Chdir(t.TempDir())
		dst := filepath.Join(".claude", "agents", "geordi.md")
		mustWrite(t, dst, mine)
		m := &project.Manifest{Files: map[string]string{dst: project.SHA([]byte(baseline))}}
		st := newTestState(false, "")
		st.stickyAll, st.stickyRes = true, resSidecar

		if err := reconcileFile(m, st, dst, catBytes, "persona"); err != nil {
			t.Fatal(err)
		}
		if got := readFile(t, dst); got != mine {
			t.Errorf("original modified: %q", got)
		}
		if got := readFile(t, dst+".new"); got != string(catBytes) {
			t.Errorf("sidecar = %q, want the shipped version", got)
		}
		if _, recorded := m.Files[dst+".new"]; recorded {
			t.Error("sidecar must not be recorded in the manifest — it's a scratch file for the user to merge")
		}
	})
}

// A read error that isn't "not exist" (here: a directory where a file is
// expected) must surface, not be mistaken for a missing file and overwritten.
func TestReconcileFile_UnreadableDestinationErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join(".claude", "agents", "geordi.md")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &project.Manifest{Files: map[string]string{}}
	st := newTestState(false, "")
	if err := reconcileFile(m, st, dst, []byte("x"), "persona"); err == nil {
		t.Error("expected an error when the destination can't be read, got nil")
	}
}

// ---------------------------------------------------------------------------
// personasToUpdate
// ---------------------------------------------------------------------------

func TestPersonasToUpdate(t *testing.T) {
	cat := catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte("g")},
		"catalog/worf/.claude/agents/worf.md":     {Data: []byte("w")},
		"catalog/data/.claude/agents/data.md":     {Data: []byte("d")},
	})

	t.Run("narrowing to a known persona returns just that one", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := personasToUpdate(cat, &project.Manifest{Files: map[string]string{}}, "worf")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != "worf" {
			t.Errorf("got %v, want [worf]", got)
		}
	})

	t.Run("narrowing to an unknown persona is an error", func(t *testing.T) {
		t.Chdir(t.TempDir())
		_, err := personasToUpdate(cat, &project.Manifest{Files: map[string]string{}}, "riker")
		if err == nil {
			t.Fatal("expected an error for an unknown persona")
		}
		if !strings.Contains(err.Error(), "riker") {
			t.Errorf("error should name the persona: %v", err)
		}
	})

	t.Run("nothing installed yields nothing to update", func(t *testing.T) {
		t.Chdir(t.TempDir())
		got, err := personasToUpdate(cat, &project.Manifest{Files: map[string]string{}}, "")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 0 {
			t.Errorf("got %v, want none", got)
		}
	})

	t.Run("picks up personas recorded in the manifest and untracked ones on disk", func(t *testing.T) {
		t.Chdir(t.TempDir())
		// geordi: recorded but file deleted (still a candidate — it gets re-added).
		// data:   on disk but never recorded (a hand-copied persona).
		// worf:   neither — not a candidate.
		mustWrite(t, project.AgentPath("data"), "hand copied\n")
		m := &project.Manifest{Files: map[string]string{project.AgentPath("geordi"): "deadbeef"}}

		got, err := personasToUpdate(cat, m, "")
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]bool{"data": true, "geordi": true}
		if len(got) != 2 {
			t.Fatalf("got %v, want exactly data+geordi", got)
		}
		for _, n := range got {
			if !want[n] {
				t.Errorf("unexpected persona %q in %v", n, got)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// promptConflict — the interactive menu is an operator-facing contract.
// ---------------------------------------------------------------------------

func TestPromptConflict_Choices(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantRes resolution
		wantAll bool
	}{
		{"empty line defaults to keep", "\n", resKeep, false},
		{"k keeps", "k\n", resKeep, false},
		{"t takes", "t\n", resTake, false},
		{"s writes a sidecar", "s\n", resSidecar, false},
		{"a keeps for all", "a\n", resKeep, true},
		{"T takes for all", "T\n", resTake, true},
		{"whitespace is trimmed", "  t  \n", resTake, false},
		{"unrecognized input re-prompts", "zzz\nt\n", resTake, false},
		{"d re-shows the diff then continues", "d\nt\n", resTake, false},
		{"eof falls back to keeping your version", "", resKeep, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := bufio.NewScanner(strings.NewReader(tc.input))
			var res resolution
			var all bool
			var err error
			out := captureStdout(t, func() {
				res, all, err = promptConflict(in, "f.md", []byte("a\n"), []byte("b\n"), "aaa", "bbb", "ccc")
			})
			if err != nil {
				t.Fatalf("promptConflict: %v", err)
			}
			if res != tc.wantRes || all != tc.wantAll {
				t.Errorf("got (res=%d all=%v), want (res=%d all=%v)", res, all, tc.wantRes, tc.wantAll)
			}
			if !strings.Contains(out, "Conflict: f.md") {
				t.Errorf("conflict header missing from prompt output:\n%s", out)
			}
		})
	}
}

// "d" must actually re-render the diff, not silently fall through.
func TestPromptConflict_RedisplayShowsDiffTwice(t *testing.T) {
	in := bufio.NewScanner(strings.NewReader("d\nk\n"))
	out := captureStdout(t, func() {
		_, _, _ = promptConflict(in, "f.md", []byte("alpha\n"), []byte("beta\n"), "a", "b", "c")
	})
	if n := strings.Count(out, "--- your version"); n != 2 {
		t.Errorf("diff rendered %d times, want 2 (initial + [d] redisplay)\n%s", n, out)
	}
}

// ---------------------------------------------------------------------------
// unifiedDiff / splitLines / short
// ---------------------------------------------------------------------------

func TestUnifiedDiff(t *testing.T) {
	t.Run("marks removed and added lines and keeps common context", func(t *testing.T) {
		got := unifiedDiff("keep\nold\ntail\n", "keep\nnew\ntail\n")
		for _, want := range []string{"  --- your version", "  +++ shipped version", "   keep", "  -old", "  +new", "   tail"} {
			if !strings.Contains(got, want) {
				t.Errorf("diff missing %q:\n%s", want, got)
			}
		}
	})

	t.Run("identical input produces no +/- lines", func(t *testing.T) {
		got := unifiedDiff("a\nb\n", "a\nb\n")
		for _, line := range strings.Split(got, "\n") {
			if strings.HasPrefix(line, "  -") && line != "  --- your version" {
				t.Errorf("unexpected removal in an identical diff: %q", line)
			}
			if strings.HasPrefix(line, "  +") && line != "  +++ shipped version" {
				t.Errorf("unexpected addition in an identical diff: %q", line)
			}
		}
	})

	t.Run("empty to non-empty is all additions", func(t *testing.T) {
		got := unifiedDiff("", "one\ntwo\n")
		if !strings.Contains(got, "  +one") || !strings.Contains(got, "  +two") {
			t.Errorf("expected both lines as additions:\n%s", got)
		}
	})

	t.Run("non-empty to empty is all removals", func(t *testing.T) {
		got := unifiedDiff("one\ntwo\n", "")
		if !strings.Contains(got, "  -one") || !strings.Contains(got, "  -two") {
			t.Errorf("expected both lines as removals:\n%s", got)
		}
	})

	// A file written on Windows (CRLF) against a catalog file with LF endings
	// must not read as "every line changed".
	t.Run("CRLF and LF versions of the same text diff clean", func(t *testing.T) {
		got := unifiedDiff("alpha\r\nbeta\r\n", "alpha\nbeta\n")
		if strings.Contains(got, "  -alpha") || strings.Contains(got, "  +alpha") {
			t.Errorf("line-ending difference alone produced a diff:\n%s", got)
		}
	})
}

func TestSplitLines(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a\n", []string{"a"}},
		{"a\nb\n", []string{"a", "b"}},
		{"a\r\nb\r\n", []string{"a", "b"}},
		{"a\n\nb\n", []string{"a", "", "b"}},
		// A file that is just a newline is one (empty) line, not zero lines.
		{"\n", []string{""}},
	}
	for _, tc := range cases {
		got := splitLines(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitLines(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitLines(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func TestShort(t *testing.T) {
	if got := short("0123456789abcdef"); got != "01234567" {
		t.Errorf("short() = %q, want 8 chars", got)
	}
	if got := short("abc"); got != "abc" {
		t.Errorf("short() truncated a short string: %q", got)
	}
	if got := short(""); got != "" {
		t.Errorf("short(\"\") = %q", got)
	}
}

// ---------------------------------------------------------------------------
// Update — flag validation happens before any filesystem work.
// ---------------------------------------------------------------------------

func TestUpdateCommand_AcceptFlagValidation(t *testing.T) {
	cases := []struct {
		accept  string
		wantErr bool
	}{
		{"ours", false},
		{"theirs", false},
		{"OURS", false},     // case-insensitive
		{" theirs ", false}, // trimmed
		{"mine", true},
		{"yes", true},
	}
	cat := catalog.New(fstest.MapFS{})
	// One scratch dir for the whole table: validation runs before any
	// filesystem work, and the valid values bail out on the empty catalog
	// without writing anything.
	t.Chdir(t.TempDir())
	for _, tc := range cases {
		t.Run(tc.accept, func(t *testing.T) {
			err := Update(cat).Run(context.Background(), []string{"update", "--accept", tc.accept})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("--accept %q was accepted, want a validation error", tc.accept)
				}
				if !strings.Contains(err.Error(), "ours|theirs") {
					t.Errorf("error should explain the allowed values: %v", err)
				}
				return
			}
			// Valid values must get past validation. They'll fail later on the
			// empty catalog / missing .shipmates dir, but never with the
			// "--accept must be" message.
			if err != nil && strings.Contains(err.Error(), "--accept must be") {
				t.Errorf("valid --accept %q was rejected: %v", tc.accept, err)
			}
		})
	}
}

// writeManaged creates missing parent directories rather than failing.
func TestWriteManaged_CreatesParents(t *testing.T) {
	t.Chdir(t.TempDir())
	dst := filepath.Join("a", "b", "c", "file.md")
	if err := writeManaged(dst, []byte("hi\n")); err != nil {
		t.Fatalf("writeManaged: %v", err)
	}
	if got := readFile(t, dst); got != "hi\n" {
		t.Errorf("content = %q", got)
	}
}
