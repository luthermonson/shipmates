package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/luthermonson/shipmates/internal/ship"
	"github.com/urfave/cli/v3"
)

// shipCommand assembles the `ship` tree from the portable supervisor
// subcommands plus whatever the platform adds on top.
//
// The supervisor half — serve/add/status/install/uninstall — depends on
// nothing but internal/ship and runs everywhere. The Fleet Commander half
// (`observe`) needs the unix-only tunnel and steer packages and is injected
// by shipcmd_unix.go. Keeping the two apart is what lets a Windows host
// supervise its projects at all; folding `observe` in here would drag the
// whole subsystem back off the platform.
func shipCommand(platform []*cli.Command) *cli.Command {
	return &cli.Command{
		Name:     "ship",
		Usage:    "supervise this host's project servers (one daemon per machine)",
		Commands: append(platform, shipSupervisorCommands()...),
	}
}

// shipSupervisorCommands is the per-host supervisor surface: one daemon
// reading ~/.shipmates/ship.yaml, keeping a project server alive in each
// listed dir. One install per machine covers every project in that file.
func shipSupervisorCommands() []*cli.Command {
	return []*cli.Command{
		{
			Name:  "serve",
			Usage: "run one coordination server per configured project and restart on crash",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "config", Usage: "ship.yaml path (default ~/.shipmates/ship.yaml)"},
				&cli.StringFlag{Name: "log-file", Usage: "append supervisor logs to this file instead of stderr"},
			},
			Action: func(ctx context.Context, c *cli.Command) error {
				if lf := c.String("log-file"); lf != "" {
					f, err := os.OpenFile(lf, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
					if err != nil {
						return err
					}
					defer f.Close()
					slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))
				}
				path := c.String("config")
				if path == "" {
					p, err := ship.ConfigPath()
					if err != nil {
						return err
					}
					path = p
				}
				conf, err := ship.LoadConfig(path)
				if err != nil {
					return fmt.Errorf("load %s: %w (list project dirs under a `projects:` key)", path, err)
				}
				return ship.Run(ctx, conf)
			},
		},
		{
			Name:      "add",
			Usage:     "add a project dir to ship.yaml",
			ArgsUsage: "<dir>",
			Action: func(ctx context.Context, c *cli.Command) error {
				dir := c.Args().First()
				if dir == "" {
					return fmt.Errorf("usage: shipmates ship add <dir>")
				}
				path, err := ship.ConfigPath()
				if err != nil {
					return err
				}
				if err := ship.AddProject(path, dir); err != nil {
					return err
				}
				fmt.Printf("added %s to %s — restart `ship serve` (or the logon task) to pick it up\n", dir, path)
				return nil
			},
		},
		{
			Name:  "status",
			Usage: "show each configured project's server state",
			Action: func(ctx context.Context, c *cli.Command) error {
				path, err := ship.ConfigPath()
				if err != nil {
					return err
				}
				conf, err := ship.LoadConfig(path)
				if err != nil {
					return err
				}
				for _, st := range ship.StatusAll(conf) {
					state := "stopped"
					if st.Running {
						state = fmt.Sprintf("running  port=%d pid=%d", st.Port, st.PID)
					}
					fmt.Printf("%-8s %s\n", state, st.Dir)
				}
				return nil
			},
		},
		{
			Name:  "install",
			Usage: "run the supervisor at logon (Windows: Scheduled Task; macOS: launchd user agent)",
			Flags: []cli.Flag{
				&cli.BoolFlag{
					Name:  "unattended",
					Usage: "start the supervisor at boot instead of at logon, so it returns after a power cut with nobody logged in (Windows only)",
				},
				&cli.BoolFlag{
					Name:  "store-password",
					Usage: "with --unattended, run the task under a stored password instead of S4U; schtasks prompts for it",
				},
			},
			Action: func(ctx context.Context, c *cli.Command) error {
				path, err := ship.ConfigPath()
				if err != nil {
					return err
				}
				if _, err := ship.LoadConfig(path); err != nil {
					return fmt.Errorf("refusing to install with a broken config: %w", err)
				}
				opts := ship.InstallOptions{
					Unattended:    c.Bool("unattended"),
					StorePassword: c.Bool("store-password"),
				}
				if opts.StorePassword && !opts.Unattended {
					return fmt.Errorf("--store-password only means something with --unattended")
				}
				if err := ship.Install(opts); err != nil {
					return err
				}
				fmt.Println("ship supervisor installed and started — logs in ~/.shipmates/ship.log")
				if opts.Unattended {
					fmt.Println("registered with a boot trigger — the ship returns on its own after a power cut")
				}
				return nil
			},
		},
		{
			Name:  "uninstall",
			Usage: "stop the supervisor and remove its logon registration",
			Action: func(ctx context.Context, c *cli.Command) error {
				if err := ship.Uninstall(); err != nil {
					return err
				}
				fmt.Println("ship supervisor removed")
				return nil
			},
		},
	}
}
