//go:build unix

package commands

import (
	"github.com/luthermonson/shipmates/internal/fleetobserver"
	"github.com/urfave/cli/v3"
)

// Fleet exposes only the capability-scoped M7-M9 production operations.
func Fleet() *cli.Command {
	return fleetWithProductionConfig(fleetobserver.ProductionConfig{})
}

func fleetWithProductionConfig(production fleetobserver.ProductionConfig) *cli.Command {
	return &cli.Command{
		Name:  "fleet",
		Usage: "authenticated Fleet observation and exact-turn operations",
		Commands: []*cli.Command{
			FleetBootstrap(), FleetEnrollment(), FleetCredentials(),
			fleetServeObserverWithConfig(production),
			fleetSteer(),
			fleetSteerTargets(),
			fleetInterrupt(),
			fleetInterruptTargets(),
			fleetObserverShips(),
			fleetObserverStatus(),
			fleetObserverEvents(),
			fleetObserverFollow(),
		},
	}
}
