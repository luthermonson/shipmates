//go:build unix && !linux && !darwin

package project

import "errors"

// BSD kernels do not expose one portable descriptor-relative atomic exchange
// primitive through Go. Refuse before staging any transaction state.
const routingAtomicExchangeSupported = false

var routingRenameExchange = func(int, string, int, string) error {
	return errors.New("atomic routing destination exchange is unavailable on this platform")
}
