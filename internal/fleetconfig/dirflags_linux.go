//go:build linux

package fleetconfig

import "golang.org/x/sys/unix"

// dirOpenFlags opens a directory with the strictest capability available:
// O_PATH grants only path-lookup rights on the fd, so a compromised code
// path cannot accidentally read or write through it.
const dirOpenFlags = unix.O_PATH | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC
