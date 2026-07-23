//go:build !linux

package commands

import (
	"context"
	"errors"

	"github.com/urfave/cli/v3"
)

func RuntimeInstall() *cli.Command {
	return &cli.Command{Name: "install", Usage: "install the verified Shipmates runtime assets", Action: func(context.Context, *cli.Command) error { return errors.New("install_unsupported_platform") }}
}
