//go:build !unix

package commands

import "github.com/urfave/cli/v3"

// platformCommands is the non-unix slice of the Fleet Commander (M1-M3)
// surface. Only Fleet stays unavailable, until the durable-mailbox port lands
// (docs/contracts/fleet-commander-*.md depend on Linux filesystem primitives).
//
// Server is not part of that subsystem, despite living next to it in the unix
// list. It is the per-project coordination server that `open`, `plan`, `live`,
// `tell`, `interrupt`, `feed`, and `show` all talk to, and that the per-host
// supervisor spawns for every registered project. Leaving it unregistered here
// did not disable those commands, it broke them: client.EnsureRunning and
// ship.runCaptain both spawn `<exe> server serve`, so on Windows the child exited
// immediately as an unknown command, EnsureRunning failed after its five-second
// poll, and the supervisor sat restarting a process that could never start.
//
// Ship is present for the same reason: only its `observe` subcommand belongs to
// Fleet Commander. The per-host supervisor underneath it —
// serve/add/status/install/uninstall — depends on nothing unix-only, and it is
// what brings a Windows host's projects back after a reboot. Gating the whole
// subsystem would have left that unreachable on the one platform that needs an
// explicit boot registration to recover at all.
func platformCommands() []*cli.Command { return []*cli.Command{Server(), Ship()} }
