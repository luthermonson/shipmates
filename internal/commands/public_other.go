//go:build !unix

package commands

import "github.com/urfave/cli/v3"

// platformCommands is the non-unix stub for Fleet Commander (M1-M3). On
// Windows/macOS the Fleet, Ship, and Server subcommands are unavailable
// until the durable-mailbox port lands (docs/contracts/fleet-commander-*.md
// depend on Linux filesystem primitives).
func platformCommands() []*cli.Command { return nil }
