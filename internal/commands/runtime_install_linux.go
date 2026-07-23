//go:build linux

package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/luthermonson/shipmates/internal/installer"
	"github.com/urfave/cli/v3"
)

// RuntimeInstall is the closed root runtime installer. It intentionally does
// not expose destination, source, service, network, or credential flags.
func RuntimeInstall() *cli.Command {
	return &cli.Command{
		Name: "install", Usage: "install the verified Shipmates runtime assets",
		Flags: []cli.Flag{
			&cli.BoolFlag{Name: "dry-run", Usage: "validate and report without changing files"},
			&cli.BoolFlag{Name: "uninstall", Usage: "remove release assets while retaining state and credentials"},
			&cli.BoolFlag{Name: "json", Usage: "emit one closed JSON report"},
			&cli.StringFlag{Name: "profile", Usage: "fixed optional profile: ubuntu-rojo-localhost"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if c.Args().Len() != 0 {
				return errors.New("install_arguments_refused")
			}
			r, err := installer.Install(installer.Options{EffectiveUID: os.Geteuid(), DryRun: c.Bool("dry-run"), Uninstall: c.Bool("uninstall"), Profile: c.String("profile"), Qualifier: installer.NewProductionQualifierFence("/")})
			if err != nil {
				return err
			}
			if c.Bool("json") {
				b, _ := json.Marshal(r)
				fmt.Println(string(b))
				return nil
			}
			fmt.Printf("shipmates: %s %s\n", r.Action, r.Release)
			return nil
		},
	}
}
