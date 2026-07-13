//go:build darwin

package project

import "golang.org/x/sys/unix"

const routingAtomicExchangeSupported = true

var routingRenameExchange = func(oldFD int, oldName string, newFD int, newName string) error {
	return unix.RenameatxNp(oldFD, oldName, newFD, newName, unix.RENAME_SWAP)
}
