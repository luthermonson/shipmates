package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
	"unicode"

	"github.com/luthermonson/shipmates/internal/fleetinterrupt"
	"github.com/luthermonson/shipmates/internal/fleetobserver"
	"github.com/luthermonson/shipmates/internal/fleetsteer"
	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/urfave/cli/v3"
)

func fleetServeObserver() *cli.Command {
	return fleetServeObserverWithConfig(fleetobserver.ProductionConfig{})
}

func fleetServeObserverWithConfig(production fleetobserver.ProductionConfig) *cli.Command {
	targetLifetime := production.InterruptTargetLifetime
	if targetLifetime == 0 {
		targetLifetime = fleetinterrupt.TargetLifetime
	}
	observationFreshness := production.InterruptObservationFreshness
	if observationFreshness == 0 {
		observationFreshness = fleetinterrupt.ObservationFreshness
	}
	return &cli.Command{Name: "serve-observer", Usage: "serve the durable authenticated read-only Fleet observer", Flags: []cli.Flag{
		&cli.StringFlag{Name: "addr", Value: "127.0.0.1:8443"},
		&cli.StringFlag{Name: "authority-store", Required: true},
		&cli.StringFlag{Name: "fleet-id", Required: true},
		&cli.StringFlag{Name: "fleet-epoch", Required: true},
		&cli.Uint64Flag{Name: "steer-epoch", Required: true},
		&cli.StringFlag{Name: "service-identity", Required: true},
		&cli.StringFlag{Name: "tls-cert", Required: true},
		&cli.StringFlag{Name: "tls-key", Required: true},
		&cli.DurationFlag{Name: "interrupt-target-lifetime", Value: targetLifetime},
		&cli.DurationFlag{Name: "interrupt-observation-freshness", Value: observationFreshness},
		&cli.DurationFlag{Name: "interrupt-subject-admission-window", Value: fleetinterrupt.SubjectAdmissionWindow},
		&cli.DurationFlag{Name: "interrupt-ship-admission-window", Value: fleetinterrupt.ShipAdmissionWindow},
		&cli.DurationFlag{Name: "interrupt-target-admission-window", Value: fleetinterrupt.TargetAdmissionWindow},
	}, Action: func(ctx context.Context, c *cli.Command) error {
		production.AuthorityStore = c.String("authority-store")
		production.FleetID = c.String("fleet-id")
		production.FleetEpoch = c.String("fleet-epoch")
		production.SteerEpoch = c.Uint64("steer-epoch")
		production.ServiceIdentity = c.String("service-identity")
		production.InterruptTargetLifetime = c.Duration("interrupt-target-lifetime")
		production.InterruptObservationFreshness = c.Duration("interrupt-observation-freshness")
		production.InterruptSubjectAdmissionWindow = c.Duration("interrupt-subject-admission-window")
		production.InterruptShipAdmissionWindow = c.Duration("interrupt-ship-admission-window")
		production.InterruptTargetAdmissionWindow = c.Duration("interrupt-target-admission-window")
		p, err := fleetobserver.OpenProduction(production)
		if err != nil {
			return err
		}
		s := &http.Server{Addr: c.String("addr"), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		return p.Serve(ctx, s, c.String("tls-cert"), c.String("tls-key"))
	}}
}

func fleetSteer() *cli.Command {
	return &cli.Command{Name: "steer", Usage: "steer one exact active remote turn", Flags: []cli.Flag{
		&cli.StringFlag{Name: "fleet", Sources: cli.EnvVars("SHIPMATES_OPERATOR_URL"), Required: true},
		&cli.StringFlag{Name: "credential-file", Sources: cli.EnvVars("SHIPMATES_OPERATOR_CREDENTIAL_FILE"), Required: true},
		&cli.StringFlag{Name: "fleet-id", Required: true}, &cli.Uint64Flag{Name: "fleet-epoch", Required: true},
		&cli.StringFlag{Name: "ship", Required: true}, &cli.Uint64Flag{Name: "connection-generation", Required: true},
		&cli.StringFlag{Name: "persona", Required: true}, &cli.StringFlag{Name: "target", Required: true},
		&cli.BoolFlag{Name: "message-stdin", Required: true}, &cli.BoolFlag{Name: "json"},
	}, Action: func(ctx context.Context, c *cli.Command) error {
		if len(c.Args().Slice()) != 0 || !c.Bool("message-stdin") {
			return errors.New("steer_requires_message_stdin")
		}
		// Validate transport before touching the credential file or stdin.
		if _, err := fleetsteer.ValidateOperatorURL(c.String("fleet")); err != nil {
			return err
		}
		cred, err := readRestrictedCredential(c.String("credential-file"))
		if err != nil {
			return err
		}
		message, err := io.ReadAll(io.LimitReader(c.Reader, 4097))
		if err != nil || len(message) == 0 || len(message) > 4096 {
			return errors.New("invalid_steer_message")
		}
		cl := fleetsteer.Client{BaseURL: c.String("fleet"), Credential: cred}
		res, err := cl.Submit(ctx, fleetsteer.SubmitV1{SchemaVersion: 1, FleetID: c.String("fleet-id"), FleetEpoch: c.Uint64("fleet-epoch"), ShipID: c.String("ship"), ConnectionGeneration: c.Uint64("connection-generation"), Persona: c.String("persona"), SteerTargetRef: c.String("target"), Message: string(message)})
		if err != nil {
			return err
		}
		if c.Bool("json") {
			if err := json.NewEncoder(c.Writer).Encode(res); err != nil {
				return err
			}
		} else {
			switch res.Outcome {
			case livesession.RemoteSteerAccepted:
				fmt.Fprintln(c.Writer, "steer accepted for this turn; effect not yet known")
			case livesession.RemoteSteerRefused:
				fmt.Fprintf(c.Writer, "steer refused: %s\n", safeTerminal(res.ReasonCode))
			default:
				fmt.Fprintln(c.Writer, "delivery outcome unknown; this message will not be retried automatically")
			}
		}
		if res.Outcome == livesession.RemoteSteerRefused {
			return steerExit{2}
		}
		if res.Outcome == livesession.RemoteSteerIndeterminate {
			return steerExit{3}
		}
		return nil
	}}
}

func fleetSteerTargets() *cli.Command {
	return &cli.Command{Name: "steer-targets", Usage: "discover bounded opaque steer targets for already-active turns", Flags: []cli.Flag{
		&cli.StringFlag{Name: "fleet", Sources: cli.EnvVars("SHIPMATES_OPERATOR_URL"), Required: true},
		&cli.StringFlag{Name: "credential-file", Sources: cli.EnvVars("SHIPMATES_OPERATOR_CREDENTIAL_FILE"), Required: true},
		&cli.BoolFlag{Name: "json"},
	}, Action: func(ctx context.Context, c *cli.Command) error {
		if len(c.Args().Slice()) != 0 {
			return errors.New("steer_targets_takes_no_arguments")
		}
		if _, err := fleetsteer.ValidateOperatorURL(c.String("fleet")); err != nil {
			return err
		}
		cred, err := readRestrictedCredential(c.String("credential-file"))
		if err != nil {
			return err
		}
		v, err := (fleetsteer.Client{BaseURL: c.String("fleet"), Credential: cred}).Targets(ctx)
		if err != nil {
			return err
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(v)
		}
		for _, target := range v.Targets {
			fmt.Fprintf(c.Writer, "%s %s %s gen=%d target=%s expires=%s\n", safeTerminal(target.ShipID), safeTerminal(target.Persona), safeTerminal(target.FleetID), target.ConnectionGeneration, safeTerminal(target.SteerTargetRef), target.ExpiresAt.UTC().Format(time.RFC3339))
		}
		return nil
	}}
}

func fleetInterrupt() *cli.Command {
	return &cli.Command{Name: "interrupt", Usage: "interrupt one exact active remote turn", Flags: []cli.Flag{
		&cli.StringFlag{Name: "fleet", Sources: cli.EnvVars("SHIPMATES_INTERRUPT_URL"), Required: true},
		&cli.StringFlag{Name: "credential-file", Sources: cli.EnvVars("SHIPMATES_INTERRUPT_CREDENTIAL_FILE"), Required: true},
		&cli.StringFlag{Name: "fleet-id"}, &cli.Uint64Flag{Name: "fleet-epoch"},
		&cli.StringFlag{Name: "ship"}, &cli.Uint64Flag{Name: "connection-generation"},
		&cli.StringFlag{Name: "persona"}, &cli.StringFlag{Name: "target"},
		&cli.StringFlag{Name: "retry-operation", Usage: "query/replay the exact caller-supplied operation ID"},
		&cli.BoolFlag{Name: "confirm"}, &cli.BoolFlag{Name: "json"},
	}, Action: func(ctx context.Context, c *cli.Command) error {
		retry := c.String("retry-operation")
		if len(c.Args().Slice()) != 0 || (retry == "" && (!c.Bool("confirm") || c.String("fleet-id") == "" || c.Uint64("fleet-epoch") == 0 || c.String("ship") == "" || c.Uint64("connection-generation") == 0 || c.String("persona") == "" || c.String("target") == "")) {
			return errors.New("interrupt_requires_exact_confirmation")
		}
		// Validate the destination before reading cancellation authority.
		if _, err := fleetinterrupt.ValidateOperatorURL(c.String("fleet")); err != nil {
			return err
		}
		cred, err := readRestrictedCredential(c.String("credential-file"))
		if err != nil {
			return err
		}
		cl := fleetinterrupt.Client{BaseURL: c.String("fleet"), Credential: cred}
		opID := retry
		var res livesession.RemoteInterruptResult
		if opID != "" {
			res, err = cl.Query(ctx, opID)
		} else {
			opID, err = fleetinterrupt.NewOperationID()
			if err != nil {
				return errors.New("interrupt_operation_id_unavailable")
			}
			res, err = cl.Submit(ctx, fleetinterrupt.SubmitV1{SchemaVersion: 1, FleetID: c.String("fleet-id"), FleetEpoch: c.Uint64("fleet-epoch"), ShipID: c.String("ship"), ConnectionGeneration: c.Uint64("connection-generation"), Persona: c.String("persona"), InterruptTargetRef: c.String("target"), OperationID: opID})
		}
		if err != nil {
			return err
		}
		if c.Bool("json") {
			if err := json.NewEncoder(c.Writer).Encode(res); err != nil {
				return err
			}
		} else {
			switch res.Outcome {
			case livesession.RemoteInterruptAccepted:
				fmt.Fprintln(c.Writer, "interrupting")
			case livesession.RemoteInterruptInterrupted:
				fmt.Fprintln(c.Writer, "interrupted")
			case livesession.RemoteInterruptAlreadyTerminal:
				fmt.Fprintln(c.Writer, "already terminal")
			case livesession.RemoteInterruptRefused:
				fmt.Fprintf(c.Writer, "interrupt refused: %s\n", safeTerminal(res.ReasonCode))
			default:
				fmt.Fprintln(c.Writer, "interrupt outcome indeterminate; do not submit a new operation automatically")
			}
		}
		if res.Outcome == livesession.RemoteInterruptRefused {
			return steerExit{2}
		}
		if res.Outcome == livesession.RemoteInterruptIndeterminate {
			return steerExit{3}
		}
		return nil
	}}
}

func fleetInterruptTargets() *cli.Command {
	return &cli.Command{Name: "interrupt-targets", Usage: "discover bounded opaque interrupt targets for already-active turns", Flags: []cli.Flag{
		&cli.StringFlag{Name: "fleet", Sources: cli.EnvVars("SHIPMATES_INTERRUPT_URL"), Required: true},
		&cli.StringFlag{Name: "credential-file", Sources: cli.EnvVars("SHIPMATES_INTERRUPT_CREDENTIAL_FILE"), Required: true},
		&cli.BoolFlag{Name: "json"},
	}, Action: func(ctx context.Context, c *cli.Command) error {
		if len(c.Args().Slice()) != 0 {
			return errors.New("interrupt_targets_takes_no_arguments")
		}
		if _, err := fleetinterrupt.ValidateOperatorURL(c.String("fleet")); err != nil {
			return err
		}
		cred, err := readRestrictedCredential(c.String("credential-file"))
		if err != nil {
			return err
		}
		v, err := (fleetinterrupt.Client{BaseURL: c.String("fleet"), Credential: cred}).Targets(ctx)
		if err != nil {
			return err
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(v)
		}
		for _, target := range v.Targets {
			fmt.Fprintf(c.Writer, "%s %s %s gen=%d target=%s expires=%s\n", safeTerminal(target.ShipID), safeTerminal(target.Persona), safeTerminal(target.FleetID), target.ConnectionGeneration, safeTerminal(target.InterruptTargetRef), target.ExpiresAt.UTC().Format(time.RFC3339))
		}
		return nil
	}}
}

type steerExit struct{ code int }

func (e steerExit) Error() string { return "" }
func (e steerExit) ExitCode() int { return e.code }

func readRestrictedCredential(path string) (string, error) {
	info, err := os.Lstat(strings.TrimSpace(path))
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("unsafe_operator_credential_file")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 512 {
		return "", errors.New("invalid_operator_credential")
	}
	v := strings.TrimSpace(string(b))
	if _, _, ok := strings.Cut(v, "."); !ok || strings.ContainsAny(v, " \t\r\n") {
		return "", errors.New("invalid_operator_credential")
	}
	return v, nil
}

func observerFlags() []cli.Flag {
	return []cli.Flag{
		&cli.StringFlag{Name: "fleet", Sources: cli.EnvVars("SHIPMATES_OBSERVER_URL"), Usage: "authenticated Fleet observer URL (HTTPS, or loopback HTTP)"},
		&cli.StringFlag{Name: "credential-file", Sources: cli.EnvVars("SHIPMATES_OBSERVER_CREDENTIAL_FILE"), Usage: "0600 file containing observer credential"},
	}
}

func observerClient(c *cli.Command) (fleetobserver.Client, error) {
	path := strings.TrimSpace(c.String("credential-file"))
	if path == "" || strings.TrimSpace(c.String("fleet")) == "" {
		return fleetobserver.Client{}, errors.New("observer_configuration_required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return fleetobserver.Client{}, errors.New("unsafe_observer_credential_file")
	}
	b, err := os.ReadFile(path)
	if err != nil || len(b) > 512 {
		return fleetobserver.Client{}, errors.New("invalid_observer_credential")
	}
	return fleetobserver.Client{BaseURL: strings.TrimRight(c.String("fleet"), "/"), Credential: strings.TrimSpace(string(b))}, nil
}

func fleetObserverShips() *cli.Command {
	return &cli.Command{Name: "ships", Usage: "list ship roster and status (read only)", Flags: append(observerFlags(), &cli.BoolFlag{Name: "json"}), Action: func(ctx context.Context, c *cli.Command) error {
		cl, e := observerClient(c)
		if e != nil {
			return e
		}
		v, e := cl.Ships(ctx)
		if e != nil {
			return e
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(v)
		}
		fmt.Fprintln(c.Writer, "Fleet (read only)")
		for _, s := range v.Ships {
			fmt.Fprintf(c.Writer, "%-8s %s  %s\n", s.Connectivity, safeTerminal(s.ShipID), safeTerminal(s.ShipLabel))
		}
		if v.Truncated {
			fmt.Fprintf(c.ErrWriter, "warning: roster truncated; %d omitted\n", v.OmittedShipCount)
		}
		return nil
	}}
}

func fleetObserverStatus() *cli.Command {
	return &cli.Command{Name: "status", ArgsUsage: "<ship_id>", Usage: "show ship and persona status (read only)", Flags: append(observerFlags(), &cli.BoolFlag{Name: "json"}), Action: func(ctx context.Context, c *cli.Command) error {
		id := c.Args().First()
		if id == "" || len(c.Args().Slice()) != 1 {
			return errors.New("usage: shipmates fleet status <ship_id> [--json]")
		}
		cl, e := observerClient(c)
		if e != nil {
			return e
		}
		v, e := cl.Status(ctx, id)
		if e != nil {
			return e
		}
		if c.Bool("json") {
			return json.NewEncoder(c.Writer).Encode(v)
		}
		fmt.Fprintf(c.Writer, "%s  %s (read only)\n", safeTerminal(v.ShipID), v.Connectivity)
		for _, p := range v.Personas {
			wait := ""
			if p.ApprovalWaiting {
				wait = "; local approval waiting"
			}
			fmt.Fprintf(c.Writer, "  %-20s session=%s turn=%s activity=%s%s\n", safeTerminal(p.Persona), p.Session, p.Turn, p.Activity, wait)
		}
		if v.Truncated {
			fmt.Fprintf(c.ErrWriter, "warning: persona list truncated; %d omitted\n", v.OmittedPersonaCount)
		}
		return nil
	}}
}

func eventFlags() []cli.Flag {
	return append(observerFlags(), &cli.StringFlag{Name: "after"}, &cli.IntFlag{Name: "limit", Value: 100}, &cli.BoolFlag{Name: "json"})
}
func fleetObserverEvents() *cli.Command {
	return &cli.Command{Name: "events", Usage: "read a bounded event page", Flags: eventFlags(), Action: func(ctx context.Context, c *cli.Command) error { return runObserverEvents(ctx, c, false) }}
}
func fleetObserverFollow() *cli.Command {
	return &cli.Command{Name: "follow", Usage: "follow events with reconnect cursors", Flags: eventFlags(), Action: func(ctx context.Context, c *cli.Command) error { return runObserverEvents(ctx, c, true) }}
}

func runObserverEvents(ctx context.Context, c *cli.Command, follow bool) error {
	cl, e := observerClient(c)
	if e != nil {
		return e
	}
	after := c.String("after")
	delay := 100 * time.Millisecond
	for {
		r, e := cl.Events(ctx, after, c.Int("limit"), follow)
		if e != nil {
			if !follow {
				return e
			}
			fmt.Fprintln(c.ErrWriter, "reconnecting: fleet unavailable")
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(delay):
			}
			if delay < 2*time.Second {
				delay *= 2
			}
			continue
		}
		delay = 100 * time.Millisecond
		if r.Gap != nil {
			fmt.Fprintf(c.ErrWriter, "gap: %s; incremental state discarded\n", r.Gap.Reason)
		}
		if c.Bool("json") {
			if e = json.NewEncoder(c.Writer).Encode(r); e != nil {
				return e
			}
		} else {
			if r.Snapshot != nil {
				fmt.Fprintf(c.Writer, "snapshot %s:%d (read only)\n", safeTerminal(r.Snapshot.FleetEpoch), r.Snapshot.SnapshotCursor)
			}
			for _, ev := range r.Events {
				fmt.Fprintf(c.Writer, "%s:%d %-20s %-18s %s\n", safeTerminal(ev.FleetEpoch), ev.Cursor, safeTerminal(ev.ShipID), ev.Kind, safeTerminal(ev.Persona))
			}
		}
		after, e = fleetobserver.AdvanceCursor(after, r)
		if e != nil {
			return e
		}
		if !follow {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(250 * time.Millisecond):
		}
	}
}

func safeTerminal(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || r == '\u001b' || r == '\u202e' || r == '\u202d' || r == '\u2066' || r == '\u2067' || r == '\u2068' || r == '\u2069' {
			b.WriteRune('?')
		} else {
			b.WriteRune(r)
		}
		if b.Len() >= 256 {
			break
		}
	}
	return b.String()
}
