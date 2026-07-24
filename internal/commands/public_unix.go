//go:build unix

package commands

import "github.com/urfave/cli/v3"

// platformCommands returns the Fleet Commander (M1-M3) subsystem's public
// CLI surface. Available on unix only because the underlying durable
// mailbox + delegation validator depend on filesystem primitives
// (openat/O_NOFOLLOW/flock) that shipmates has not yet ported to Windows.
func platformCommands() []*cli.Command {
	return []*cli.Command{Fleet(), Server(), Ship()}
}
