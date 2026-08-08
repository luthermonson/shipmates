//go:build unix

package recovery

import "os"

// journalWorldAccessible reports whether the journal file is readable or
// writable by anyone other than its owner. The journal carries redacted
// provenance only, but it is still an authority-relevant record: a
// group/other-writable journal could have blocker or assessment records
// planted into it.
func journalWorldAccessible(info os.FileInfo) bool {
	return info.Mode().Perm()&0o077 != 0
}
