//go:build unix

package livesession

import (
	"errors"
	"os"
	"path/filepath"
)

// errInsecureState is the single internal signal that a durable-state path did
// not meet its security contract. Callers translate it into their own
// subsystem's opaque message ("remote steer storage unavailable", …) so the
// wire surface never describes the filesystem.
var errInsecureState = errors.New("insecure durable state")

// secureStateDir requires path to be an absolute, already-normalized directory
// whose leaf is unreachable by group or other and where no component anywhere up
// to the filesystem root is a symlink. With create it makes the missing
// components 0700 on the way down.
//
// Checking the whole ancestry matters because the leaf's own mode is worthless if
// a parent can be swapped for a symlink into somebody else's tree between the
// check and the open.
func secureStateDir(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errInsecureState
	}
	if create {
		// Walk up to the filesystem root: the fixed point of filepath.Dir.
		// Comparing against filepath.Separator alone never terminates on
		// Windows, where the walk bottoms out at a volume root like `C:\`.
		parts := []string{}
		p := path
		for {
			parent := filepath.Dir(p)
			if parent == p {
				break
			}
			parts = append(parts, filepath.Base(p))
			p = parent
		}
		for i := len(parts) - 1; i >= 0; i-- {
			p = filepath.Join(p, parts[i])
			st, err := os.Lstat(p)
			if errors.Is(err, os.ErrNotExist) {
				if os.Mkdir(p, 0o700) != nil {
					return errInsecureState
				}
				st, err = os.Lstat(p)
			}
			if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				return errInsecureState
			}
		}
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errInsecureState
		}
		if p == path && st.Mode().Perm()&0o077 != 0 {
			return errInsecureState
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}

// verifyPrivateStateFile requires path to name an existing regular file — not a
// symlink, not a directory — that grants nothing to group or other. A missing
// file is reported as os.ErrNotExist so callers can treat absence as a first
// run.
func verifyPrivateStateFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return errInsecureState
	}
	return nil
}

// createPrivateStateFile creates path exclusively, private from birth, open for
// writing. It fails if path already exists, which is what makes the
// stage-then-rename in the durable stores safe to retry.
func createPrivateStateFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
}

// openPrivateAppendFile opens path for append, creating it privately if absent,
// and refuses to hand back a descriptor that is not a private regular file. The
// audit sink holds this open for the process lifetime, so the check is on the
// descriptor rather than the name.
func openPrivateAppendFile(path string) (*os.File, error) {
	if err := verifyPrivateStateFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		_ = f.Close()
		return nil, errInsecureState
	}
	return f, nil
}
