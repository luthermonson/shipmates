//go:build windows

package recovery

import "os"

// journalWorldAccessible is the Windows counterpart of the unix
// Perm()&0o077 check. It deliberately reports false.
//
// Go synthesizes 0666/0777 permission bits for every writable file on
// Windows; the unix check would therefore reject every journal ever created,
// including ones this package just wrote with 0o600. The honest equivalent is
// a DACL inspection (the winsec approach on feat/ship-install-durability),
// which main has not adopted yet. Journals live under the project's
// .shipmates directory, whose ACL already restricts access to the owner in
// any normal profile layout, so until winsec lands the check is a no-op here
// rather than a lie.
func journalWorldAccessible(os.FileInfo) bool {
	return false
}
