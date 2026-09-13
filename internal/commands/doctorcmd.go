package commands

import (
	"context"
	"fmt"

	"github.com/luthermonson/shipmates/internal/doctor"
	"github.com/urfave/cli/v3"
)

// Doctor wires the read-only environment/config diagnostic into the command
// tree as a bare, run-once command (like `bridge`, not a grouped command like
// `server`). It runs every registered check, prints them grouped with
// OK/WARN/FAIL markers and a summary, and exits non-zero iff any check FAILed —
// so it is usable as a pre-flight or CI gate. Warn never fails the command.
//
// The command is strictly read-only: it creates, modifies and deletes nothing,
// starts no process beyond cheap read-only probes, and never prints a secret
// value (only whether an env var is set).
func Doctor() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "read-only diagnostic of the shipmates environment and configuration",
		Description: "Runs a registry of read-only checks across the toolchain (claude/git/bd),\n" +
			"the operator config under ~/.shipmates/, the selected runtime, the captain\n" +
			"server's on-disk state and supervisor install status, the fleet wiring, and\n" +
			"installed personas. Each check prints OK, WARN or FAIL.\n" +
			"\n" +
			"Exit code is non-zero iff any check FAILs (WARN does not fail), so `shipmates\n" +
			"doctor` works as a pre-flight or CI hook. It changes nothing on disk and\n" +
			"never prints a secret value — env-var-held credentials are reported only as\n" +
			"set or unset.",
		Flags: []cli.Flag{runtimeFlag()},
		Action: func(_ context.Context, c *cli.Command) error {
			e := doctor.Production("", c.String("runtime"))
			results := doctor.Run(e)
			summary := doctor.Render(c.Writer, results)
			if summary.Failed() {
				// Non-zero exit for CI/pre-flight. The results are already
				// printed; keep the error terse.
				return cli.Exit(fmt.Sprintf("doctor: %d check(s) failed", summary.Fail), 1)
			}
			return nil
		},
	}
}
