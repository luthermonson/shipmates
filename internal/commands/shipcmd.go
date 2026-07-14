package commands

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/fleetobserve"
	"github.com/luthermonson/shipmates/internal/fleetsteer"
	"github.com/luthermonson/shipmates/internal/fleettunnel"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/ship"
	"github.com/urfave/cli/v3"
)

// Ship manages the per-host supervisor: one daemon that keeps a project server
// alive in every project dir listed in ~/.shipmates/ship.yaml.
func Ship() *cli.Command {
	return shipWithProductionReadiness(nil, nil)
}

func shipWithProductionClock(clock fleetsteer.Clock) *cli.Command {
	return shipWithProductionReadiness(clock, nil)
}

func shipWithProductionReadiness(interruptClock fleetsteer.Clock, ready chan<- struct{}) *cli.Command {
	return &cli.Command{
		Name:  "ship",
		Usage: "supervise this host's project servers (one daemon per machine)",
		Commands: []*cli.Command{
			{
				Name: "observe", Usage: "publish this project's read-only state through the authenticated Fleet tunnel",
				Flags: []cli.Flag{&cli.StringFlag{Name: "project", Value: "."}, &cli.StringFlag{Name: "identity-store", Required: true}, &cli.Uint64Flag{Name: "steer-epoch", Required: true}},
				Action: func(ctx context.Context, c *cli.Command) error {
					root, err := project.CanonicalRoot(c.String("project"))
					if err != nil {
						return err
					}
					adapter, err := fleetobserve.OpenLocalStateAdapter(root)
					if err != nil {
						return err
					}
					states := make([]fleetobserve.LocalPersonaState, adapter.SlotCount())
					for i := range states {
						states[i] = fleetobserve.LocalPersonaState{Session: fleetobserve.SessionAbsent, Turn: fleetobserve.TurnNone, Activity: fleetobserve.ActivityIdle}
					}
					// Seed the Fleet projection from the server-owned live-session
					// manager. The opaque target API is the production authority for
					// active turns; filesystem/session-marker inference is not.
					control, err := fleetsteer.DiscoverLocalControl(ctx, root)
					if err != nil {
						return err
					}
					targets, err := control.Targets(ctx)
					if err != nil {
						return err
					}
					active := make(map[string]bool, len(targets))
					for _, target := range targets {
						active[target.Persona] = true
					}
					activeNames := make([]string, 0, len(active))
					for persona := range active {
						activeNames = append(activeNames, persona)
					}
					states, err = adapter.StatesForActivePersonas(activeNames)
					if err != nil {
						return err
					}
					if err := adapter.ValidateInventory(root); err != nil {
						return err
					}
					return runShipObserveReadyWithClock(ctx, root, c.String("identity-store"), c.Uint64("steer-epoch"), states, ready, interruptClock)
				},
			},
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
				Action: func(ctx context.Context, c *cli.Command) error {
					path, err := ship.ConfigPath()
					if err != nil {
						return err
					}
					if _, err := ship.LoadConfig(path); err != nil {
						return fmt.Errorf("refusing to install with a broken config: %w", err)
					}
					if err := ship.Install(); err != nil {
						return err
					}
					fmt.Println("ship supervisor installed and started — logs in ~/.shipmates/ship.log")
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
		},
	}
}

func runShipObserve(ctx context.Context, root, identityDir string, steerEpoch uint64, states []fleetobserve.LocalPersonaState) error {
	return runShipObserveWithClock(ctx, root, identityDir, steerEpoch, states, nil)
}

func runShipObserveReady(ctx context.Context, root, identityDir string, steerEpoch uint64, states []fleetobserve.LocalPersonaState, ready chan<- struct{}) error {
	return runShipObserveReadyWithClock(ctx, root, identityDir, steerEpoch, states, ready, nil)
}

func runShipObserveWithClock(ctx context.Context, root, identityDir string, steerEpoch uint64, states []fleetobserve.LocalPersonaState, interruptClock fleetsteer.Clock) error {
	return runShipObserveReadyWithClock(ctx, root, identityDir, steerEpoch, states, nil, interruptClock)
}

func runShipObserveReadyWithClock(ctx context.Context, root, identityDir string, steerEpoch uint64, states []fleetobserve.LocalPersonaState, ready chan<- struct{}, interruptClock fleetsteer.Clock) error {
	if steerEpoch == 0 {
		return fmt.Errorf("invalid_ship_runtime")
	}
	control, err := fleetsteer.DiscoverLocalControl(ctx, root)
	if err != nil {
		return err
	}
	adapter, err := fleetobserve.OpenLocalStateAdapter(root)
	if err != nil {
		return err
	}
	updates := make(chan []fleetobserve.LocalPersonaState, 1)
	_, _, err = fleettunnel.RunProductionLocalConnectedUpdates(ctx, root, identityDir, states, fleettunnel.Resume{}, nil, updates, func(c context.Context, id fleetidentity.ShipState, g uint64) (func(), error) {
		ep, e := fleetsteer.NewProductionShipEndpoint(id.FleetID, id.ShipID, steerEpoch, g, fleetsteer.ShipEndpointConfig{InterruptClock: interruptClock}, control)
		if e != nil {
			return nil, e
		}
		stop := make(chan struct{})
		targetReady := make(chan error, 1)
		go maintainSteerTarget(stop, control, adapter, id, steerEpoch, g, ep, updates, targetReady)
		select {
		case targetErr := <-targetReady:
			if targetErr != nil {
				close(stop)
				return nil, targetErr
			}
		case <-time.After(5 * time.Second):
			close(stop)
			return nil, fmt.Errorf("steer_target_timeout")
		case <-c.Done():
			close(stop)
			return nil, c.Err()
		}
		connCtx, cancel := context.WithCancel(c)
		done := make(chan error, 1)
		reverseReady := make(chan struct{})
		go func() {
			done <- fleetsteer.RunReverseClientInterruptReady(connCtx, id.FleetDestination, id, g, ep, func() { close(reverseReady) })
		}()
		select {
		case <-reverseReady:
		case reverseErr := <-done:
			close(stop)
			cancel()
			return nil, fmt.Errorf("reverse_tunnel_unavailable: %w", reverseErr)
		case <-time.After(5 * time.Second):
			close(stop)
			cancel()
			<-done
			return nil, fmt.Errorf("reverse_tunnel_timeout")
		case <-c.Done():
			close(stop)
			cancel()
			<-done
			return nil, c.Err()
		}
		if ready != nil {
			close(ready)
		}
		return func() { close(stop); ep.InvalidateTargets(); cancel(); <-done }, nil
	})
	return err
}

func maintainSteerTarget(stop <-chan struct{}, control *fleetsteer.LocalControl, adapter *fleetobserve.LocalStateAdapter, id fleetidentity.ShipState, epoch, generation uint64, ep *fleetsteer.ShipEndpoint, updates chan<- []fleetobserve.LocalPersonaState, initial chan<- error) {
	t := time.NewTicker(25 * time.Millisecond)
	defer t.Stop()
	var installed string
	var projected string
	first := true
	for {
		refresh := func() {
			targets, err := control.Targets(context.Background())
			refs := make([]string, 0, len(targets))
			if err == nil {
				for i := range targets {
					targets[i].FleetID, targets[i].FleetEpoch, targets[i].ShipID, targets[i].ConnectionGeneration = id.FleetID, epoch, id.ShipID, generation
					refs = append(refs, targets[i].Reference)
				}
				sort.Strings(refs)
			}
			found := strings.Join(refs, "\x00")
			if err == nil && found != installed {
				aliases := make(map[string]string, len(targets))
				for _, target := range targets {
					opaque, aliasErr := adapter.PersonaReference(id.ShipID, target.Persona)
					if aliasErr != nil {
						ep.InvalidateTargets()
						return
					}
					aliases[opaque] = target.Persona
				}
				if ep.InstallTargetsAndInterruptPersonaAliases(targets, aliases) == nil {
					installed = found
				}
			}
			if err != nil || found != installed {
				ep.InvalidateTargets()
				installed = ""
			}
			if err == nil && installed == found && found != projected {
				personas := make([]string, 0, len(targets))
				for _, target := range targets {
					personas = append(personas, target.Persona)
				}
				next, stateErr := adapter.StatesForActivePersonas(personas)
				if stateErr != nil {
					ep.InvalidateTargets()
					installed = ""
				} else {
					select {
					case updates <- next:
						projected = found
					default:
					}
				}
			}
			if first {
				first = false
				if err != nil {
					initial <- fmt.Errorf("local_target_discovery_failed: %w", err)
				} else {
					initial <- nil
				}
			}
		}
		refresh()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
