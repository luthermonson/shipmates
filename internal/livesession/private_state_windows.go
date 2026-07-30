//go:build windows

package livesession

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/luthermonson/shipmates/internal/winsec"
	"golang.org/x/sys/windows"
)

// errInsecureState is the single internal signal that a durable-state path did
// not meet its security contract. Callers translate it into their own
// subsystem's opaque message ("remote steer storage unavailable", …) so the
// wire surface never describes the filesystem.
var errInsecureState = errors.New("insecure durable state")

// windowsStatePathParts splits a fully qualified path into the volume (or UNC
// share) root and the literal names below it.
//
// Windows path handling is the reason this is a separate function rather than a
// strings.Split: `C:` and `C:\` name different things (the former is the current
// directory on drive C), a UNC root is two components deep before any real
// directory appears, and the `\\?\` form winsec produces is handed to the object
// manager verbatim, so a component that is not a single literal name must never
// reach it.
func windowsStatePathParts(path string) (string, []string, bool) {
	volume := filepath.VolumeName(path)
	if volume == "" {
		return "", nil, false
	}
	rest := strings.Trim(path[len(volume):], `\`)
	if rest == "" {
		return "", nil, false
	}
	return volume + `\`, strings.Split(rest, `\`), true
}

// secureStateDir requires path to be an absolute, already-normalized directory
// that no account other than its owner and LOCAL SYSTEM can reach and where no
// component anywhere up to the volume root is a reparse point. With create it
// makes the missing components on the way down.
//
// This is the Windows analogue of the unix implementation's "every component is
// a real directory, no symlinks, leaf is not group- or other-readable", and it is
// worth being exact about how the two differ:
//
//   - unix reads nine mode bits on the leaf. That says nothing about ACLs, and
//     nothing at all about the ancestors' permissions.
//   - here the leaf is given a *protected* DACL granting full control to the
//     process user and LOCAL SYSTEM and to nobody else, which winsec writes and
//     then reads back before returning. Protected means inheritance is severed,
//     so a permissive grant on an ancestor cannot flow down into it, and the
//     entries are marked inheritable so every file created inside starts private
//     too. Like the unix leaf-mode check this is self-healing rather than
//     fail-closed: a directory somebody widened is narrowed back, and only a
//     directory that cannot be narrowed is refused.
//   - ancestors are walked with winsec.OpenDirChain, which opens each component
//     with FILE_FLAG_OPEN_REPARSE_POINT and refuses it if it is a reparse point
//     (the O_NOFOLLOW equivalent), verifies that the handle canonicalizes back to
//     the name that was asked for, and holds every handle open without
//     FILE_SHARE_DELETE so a component already proven to be a real directory
//     cannot be renamed away and replaced by a junction while the walk continues
//     below it. That is what supplies openat's "resolve relative to something I
//     already validated" property, which Windows has no direct primitive for.
//
// Net: strictly stronger than the unix check on the leaf, and equivalent on the
// ancestry. It is not the *same* guarantee — Windows has no mode bits to compare
// — but nothing is skipped and nothing is assumed.
func secureStateDir(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errInsecureState
	}
	root, parts, ok := windowsStatePathParts(path)
	if !ok {
		return errInsecureState
	}
	if create {
		p := root
		for _, part := range parts {
			p = filepath.Join(p, part)
			if _, err := os.Lstat(p); err == nil {
				// Already there. Not attempting the Mkdir matters near the volume
				// root, where CreateDirectory on an existing directory the caller
				// may not write to can answer ERROR_ACCESS_DENIED rather than
				// ERROR_ALREADY_EXISTS. What each component *is* gets proven by the
				// chain walk below either way.
				continue
			}
			// Mode is ignored on Windows; the DACL below is what makes the leaf
			// private, and the leaf is the only component this package owns.
			if err := os.Mkdir(p, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
				return errInsecureState
			}
		}
	}
	chain, err := winsec.OpenDirChain(root, parts...)
	if err != nil {
		return errInsecureState
	}
	defer chain.Close()
	// OpenDirChain asks only for GENERIC_READ, and SetSecurityInfo needs
	// WRITE_DAC, so the leaf is reopened by its canonical path — safe to do by
	// name because the chain is still pinning every ancestor.
	leaf, _, err := winsec.Open(chain.Path, true,
		windows.GENERIC_READ|windows.READ_CONTROL|windows.WRITE_DAC, windows.OPEN_EXISTING)
	if err != nil {
		return errInsecureState
	}
	defer windows.CloseHandle(leaf)
	if err := winsec.VerifyFinalPath(leaf, chain.Path); err != nil {
		return errInsecureState
	}
	if err := winsec.PrivateDACL(leaf, true); err != nil {
		return errInsecureState
	}
	return nil
}

// verifyPrivateStateFile requires path to name an existing regular file — not a
// reparse point, not a directory, not hard-linked under a second name — whose
// DACL is exactly the private one. A missing file is reported as os.ErrNotExist
// (windows.ERROR_FILE_NOT_FOUND satisfies errors.Is for it) so callers can treat
// absence as a first run.
func verifyPrivateStateFile(path string) error {
	h, _, err := winsec.Open(path, false, windows.GENERIC_READ|windows.READ_CONTROL, windows.OPEN_EXISTING)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)
	if winsec.VerifyPrivateDACL(h, false) != nil {
		return errInsecureState
	}
	return nil
}

// createPrivateStateFile creates path exclusively, private from birth, open for
// writing. CREATE_NEW is the O_CREAT|O_EXCL equivalent, which is what makes the
// stage-then-rename in the durable stores safe to retry.
//
// The DACL is written explicitly rather than left to inheritance from the
// directory secureStateDir hardened: inheritance would produce the same entries
// today, but a file whose privacy has been proven on its own handle does not
// depend on that remaining true.
func createPrivateStateFile(path string) (*os.File, error) {
	h, _, err := winsec.Open(path, false,
		windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL|windows.WRITE_DAC,
		windows.CREATE_NEW)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	if err := winsec.PrivateDACL(windows.Handle(f.Fd()), false); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return f, nil
}

// openPrivateAppendFile opens path for append, creating it privately if absent,
// and refuses to hand back a handle that is not a private regular file.
//
// FILE_APPEND_DATA without FILE_WRITE_DATA is the O_APPEND equivalent and the
// stronger half of it: the handle physically cannot overwrite an existing byte,
// which is the property an audit log wants. FlushFileBuffers is documented to
// want GENERIC_WRITE but is accepted on such a handle, so the sink's per-record
// Sync still commits.
//
// The sink holds this handle for the process lifetime, so it is opened with
// FILE_SHARE_DELETE — see winsec.OpenShared. Without it the audit file could not
// be rotated or removed until the server exits, which is not how the unix
// implementation behaves and would leave an undeletable file behind.
func openPrivateAppendFile(path string) (*os.File, error) {
	h, _, err := winsec.OpenShared(path, false,
		windows.FILE_APPEND_DATA|windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES|
			windows.READ_CONTROL|windows.WRITE_DAC,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.OPEN_ALWAYS)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(h), path)
	if err := winsec.PrivateDACL(windows.Handle(f.Fd()), false); err != nil {
		_ = f.Close()
		return nil, errInsecureState
	}
	return f, nil
}
