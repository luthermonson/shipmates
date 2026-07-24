package commands

import (
	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/urfave/cli/v3"
)

// PublicCommands is the complete ordered product command surface.
//
// Fleet Commander (M1-M3) commands — Fleet, Ship, Server — are only
// registered on unix platforms where the durable-mailbox subsystem is
// available. See platformCommands / platformCommands_other.
func PublicCommands(cat *catalog.Catalog) []*cli.Command {
	base := []*cli.Command{
		Init(cat), Policy(), Add(cat), List(cat), Remove(), Update(cat),
		Render(cat), Routing(cat), Open(), Ask(), Live(), Tell(), Feed(), Interrupt(),
		Fanout(), Drain(cat), DrainMany(cat), Autonomous(cat), Beads(), Plan(), Sail(),
		Hook(),
	}
	return append(base, platformCommands()...)
}
