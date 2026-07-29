//go:build !unix

package commands

import "github.com/urfave/cli/v3"

// platformCommands is the non-unix slice of the Fleet Commander (M1-M3)
// surface. Fleet and Server stay unavailable until the durable-mailbox port
// lands (docs/contracts/fleet-commander-*.md depend on Linux filesystem
// primitives).
//
// Ship is the exception, and deliberately so: only its `observe` subcommand
// belongs to Fleet Commander. The per-host supervisor underneath it —
// serve/add/status/install/uninstall — depends on nothing unix-only, and it
// is what brings a Windows host's projects back after a reboot. Gating the
// whole subsystem would have left that unreachable on the one platform that
// needs an explicit boot registration to recover at all.
func platformCommands() []*cli.Command { return []*cli.Command{Ship()} }
