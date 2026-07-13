//go:build linux

package project

import "golang.org/x/sys/unix"

const routingAtomicExchangeSupported = true

var routingRenameExchange = func(oldFD int, oldName string, newFD int, newName string) error {
	return unix.Renameat2(oldFD, oldName, newFD, newName, unix.RENAME_EXCHANGE)
}
