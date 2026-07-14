package commands

import (
	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/urfave/cli/v3"
)

// PublicCommands is the complete ordered product command surface.
func PublicCommands(cat *catalog.Catalog) []*cli.Command {
	return []*cli.Command{
		Init(cat), Policy(), Add(cat), List(cat), Remove(), Update(cat),
		Render(cat), Routing(cat), Open(), Ask(), Live(), Tell(), Feed(), Interrupt(),
		Fanout(), Drain(cat), DrainMany(cat), Autonomous(cat), Beads(), Plan(), Sail(), Fleet(), Server(), Ship(),
	}
}
