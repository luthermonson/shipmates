//go:build unix && !linux

package fleetconfig

import "golang.org/x/sys/unix"

// dirOpenFlags for non-Linux unix. O_PATH is Linux-only; substitute a
// standard read-open with the same symlink-safe (O_NOFOLLOW) and
// directory-only (O_DIRECTORY) guards. The resulting fd is used only for
// openat + fstat here, never read from, so the TOCTOU-safety guarantee is
// unchanged. The one lost property is O_PATH's kernel-side confinement of
// what the fd can do — a defense-in-depth measure, not the primary
// security control.
const dirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
