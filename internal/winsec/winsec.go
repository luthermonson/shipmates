// Package winsec holds the Windows filesystem security primitives shipmates
// uses where unix code reaches for openat/O_NOFOLLOW, flock, and fstat
// identity comparison.
//
// Every symbol lives in winsec_windows.go. On other platforms the package is
// deliberately empty so that `go build ./...` still succeeds everywhere while
// callers stay behind `_windows.go` build tags.
package winsec
