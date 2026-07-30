//go:build unix

package commands

import "github.com/urfave/cli/v3"

// platformCommands returns the Fleet Commander (M1-M3) subsystem's public
// CLI surface plus the two commands that only sit next to it. Fleet is
// available on unix only because the underlying durable mailbox + delegation
// validator depend on filesystem primitives (openat/O_NOFOLLOW/flock) that
// shipmates has not yet ported to Windows; Server and Ship are cross-platform
// and public_other.go registers both — see the comment there.
func platformCommands() []*cli.Command {
	return []*cli.Command{Fleet(), Server(), Ship()}
}
