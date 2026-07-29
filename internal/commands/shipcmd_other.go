//go:build !unix

package commands

import "github.com/urfave/cli/v3"

// Ship manages the per-host supervisor: one daemon that keeps a project
// server alive in every project dir listed in ~/.shipmates/ship.yaml.
//
// The Fleet Commander `observe` subcommand is absent here — it is built on
// the unix-only tunnel and steer packages. The supervisor itself is
// portable, and on Windows it is the thing that keeps projects running
// across a reboot, so it is deliberately still offered.
func Ship() *cli.Command { return shipCommand(nil) }
