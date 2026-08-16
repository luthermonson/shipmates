package berth

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

// hostilePersonas mirrors the project package's list: the shapes a persona
// name arrives in when it came from a URL wildcard. A berth path is the
// argument to `git worktree add`, so an escape here creates a checkout
// wherever the caller pointed.
var hostilePersonas = []string{
	"../escape",
	"..",
	"../../../../tmp/pwned",
	"nested/name",
	"nested\\name",
	"-rf",
	"--force",
	"/absolute",
	"Captain",
	"with space",
	"",
}

// TestDirRefusesIllegalPersona: Dir must keep the result under
// .shipmates/berths and must not produce a path git could act on.
func TestDirRefusesIllegalPersona(t *testing.T) {
	parent := filepath.Join(project.Dir, "berths")
	for _, p := range hostilePersonas {
		got := Dir(p)
		if filepath.Dir(got) != parent {
			t.Errorf("Dir(%q) = %q, escaped %q", p, got, parent)
		}
		if strings.Contains(filepath.ToSlash(got), "..") {
			t.Errorf("Dir(%q) = %q, still carries a parent hop", p, got)
		}
		if err := os.MkdirAll(got, 0o755); err == nil {
			t.Errorf("Dir(%q) = %q, which was creatable", p, got)
			_ = os.RemoveAll(got)
		}
	}
	if got, want := Dir("captain"), filepath.Join(project.Dir, "berths", "captain"); got != want {
		t.Fatalf("Dir(captain) = %q, want %q", got, want)
	}
}

// TestEnsureAndRemoveRefuseIllegalPersona: the two operations that shell out
// to git refuse by name, before IsGitRepo, and create nothing.
func TestEnsureAndRemoveRefuseIllegalPersona(t *testing.T) {
	root := t.TempDir()
	sandbox := filepath.Join(root, "ship")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{sandbox, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(sandbox)

	for _, p := range hostilePersonas {
		for _, policy := range []Policy{PolicyAuto, PolicyRequire} {
			path, err := Ensure(p, policy)
			if !errors.Is(err, ErrInvalidPersona) {
				t.Errorf("Ensure(%q, %s) = (%q, %v), want ErrInvalidPersona", p, policy, path, err)
			}
			if path != "" {
				t.Errorf("Ensure(%q, %s) returned a path %q", p, policy, path)
			}
		}
		if err := Remove(p, true); !errors.Is(err, ErrInvalidPersona) {
			t.Errorf("Remove(%q) = %v, want ErrInvalidPersona", p, err)
		}
	}

	var created []string
	err := filepath.WalkDir(root, func(pth string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if pth != root && pth != sandbox && pth != outside {
			created = append(created, pth)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != 0 {
		t.Fatalf("illegal persona names created filesystem entries: %v", created)
	}
}

// TestEnsureOffStillShortCircuits: policy off returns before any validation,
// because it does not build a path at all. Keeps the fleet default (personas
// with no berth, whatever they are called) working unchanged.
func TestEnsureOffStillShortCircuits(t *testing.T) {
	path, err := Ensure("../escape", PolicyOff)
	if err != nil || path != "" {
		t.Fatalf("Ensure(_, off) = (%q, %v), want (\"\", nil)", path, err)
	}
}
