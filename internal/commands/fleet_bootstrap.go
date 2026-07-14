package commands

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

const bootstrapSchema = 1

type enrollmentFile struct {
	SchemaVersion int       `json:"schema_version"`
	FleetID       string    `json:"fleet_id"`
	ArtifactID    string    `json:"artifact_id"`
	Secret        string    `json:"secret"`
	ExpiresAt     time.Time `json:"expires_at"`
}

type credentialFile struct {
	SchemaVersion int      `json:"schema_version"`
	Kind          string   `json:"kind"`
	FleetID       string   `json:"fleet_id"`
	CredentialID  string   `json:"credential_id"`
	Generation    uint64   `json:"generation,omitempty"`
	SubjectID     string   `json:"subject_id,omitempty"`
	ShipIDs       []string `json:"ship_ids,omitempty"`
	Secret        string   `json:"secret"`
}

type bootstrapMetadata struct {
	SchemaVersion int    `json:"schema_version"`
	FleetID       string `json:"fleet_id"`
	Authority     string `json:"authority_store"`
}

type enrollmentMetadata struct {
	SchemaVersion int       `json:"schema_version"`
	FleetID       string    `json:"fleet_id"`
	ShipID        string    `json:"ship_id"`
	CredentialID  string    `json:"credential_id"`
	IdentityStore string    `json:"identity_store"`
	ExpiresAt     time.Time `json:"enrollment_expires_at"`
}

// FleetBootstrap adds only the durable authority and credential bootstrap
// surface. Secrets are accepted from protected files/stdin and are written
// only to explicit create-new 0600 files.
func FleetBootstrap() *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "initialize a durable Fleet authority",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "authority-store", Required: true},
			&cli.StringFlag{Name: "fleet-id", Required: true},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			if _, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil); err != nil {
				return err
			}
			return json.NewEncoder(c.Writer).Encode(bootstrapMetadata{bootstrapSchema, c.String("fleet-id"), c.String("authority-store")})
		},
	}
}

func FleetEnrollment() *cli.Command {
	return &cli.Command{Name: "enrollment", Usage: "create or consume one-use Fleet enrollment artifacts", Commands: []*cli.Command{
		{
			Name: "create", Usage: "create a short-lived enrollment artifact",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true},
				&cli.DurationFlag{Name: "ttl", Value: 15 * time.Minute}, &cli.StringFlag{Name: "output", Required: true},
			},
			Action: func(ctx context.Context, c *cli.Command) error {
				r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
				if err != nil {
					return err
				}
				a, err := r.CreateEnrollment(c.Duration("ttl"))
				if err != nil {
					return err
				}
				b, err := json.Marshal(enrollmentFile{bootstrapSchema, a.FleetID, a.ArtifactID, a.Secret, a.ExpiresAt})
				if err != nil {
					return errors.New("enrollment_output_failed")
				}
				if err := createSecretFile(c.String("output"), append(b, '\n')); err != nil {
					return err
				}
				return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": bootstrapSchema, "fleet_id": a.FleetID, "artifact_id": a.ArtifactID, "expires_at": a.ExpiresAt})
			},
		},
		{
			Name: "consume", Usage: "consume an enrollment artifact into an external ship identity store",
			Flags: []cli.Flag{
				&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true},
				&cli.StringFlag{Name: "enrollment-file"}, &cli.BoolFlag{Name: "enrollment-stdin"},
				&cli.StringFlag{Name: "identity-store", Required: true}, &cli.StringFlag{Name: "fleet-destination", Required: true}, &cli.StringFlag{Name: "fleet-service-identity", Required: true},
			},
			Action: func(ctx context.Context, c *cli.Command) error {
				if (c.String("enrollment-file") == "") == !c.Bool("enrollment-stdin") {
					return errors.New("exactly_one_enrollment_input_required")
				}
				r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
				if err != nil {
					return err
				}
				var raw []byte
				if c.Bool("enrollment-stdin") {
					raw, err = io.ReadAll(io.LimitReader(c.Reader, 16<<10))
				} else {
					raw, err = readProtectedFile(c.String("enrollment-file"))
				}
				if err != nil {
					return errors.New("invalid_enrollment_input")
				}
				var in enrollmentFile
				dec := json.NewDecoder(strings.NewReader(string(raw)))
				dec.DisallowUnknownFields()
				if dec.Decode(&in) != nil || in.SchemaVersion != bootstrapSchema || in.FleetID != c.String("fleet-id") || in.ArtifactID == "" || in.Secret == "" {
					return errors.New("invalid_enrollment_input")
				}
				if !c.Bool("enrollment-stdin") {
					if err := os.Remove(c.String("enrollment-file")); err != nil {
						return errors.New("enrollment_input_cleanup_failed")
					}
				}
				txn, err := randomID("txn_")
				if err != nil {
					return errors.New("enrollment_transaction_unavailable")
				}
				res, err := r.Enroll(in.ArtifactID, in.Secret, txn)
				if err != nil {
					return err
				}
				state := fleetidentity.ShipState{SchemaVersion: fleetidentity.ShipStateSchema, FleetID: res.FleetID, ShipID: res.ShipID, FleetDestination: c.String("fleet-destination"), FleetServiceIdentity: c.String("fleet-service-identity"), CredentialID: res.Credential.CredentialID, CredentialSecret: res.Credential.Secret}
				if err := rejectRepositoryPath(c.String("identity-store")); err != nil {
					_ = r.RevokeShip(res.ShipID)
					return err
				}
				if err := fleetidentity.StoreShipState(c.String("identity-store"), state); err != nil {
					_ = r.RevokeShip(res.ShipID)
					return errors.New("ship_identity_commit_failed")
				}
				return json.NewEncoder(c.Writer).Encode(enrollmentMetadata{bootstrapSchema, res.FleetID, res.ShipID, res.Credential.CredentialID, c.String("identity-store"), in.ExpiresAt})
			},
		},
	}}
}

func FleetCredentials() *cli.Command {
	return &cli.Command{Name: "credential", Usage: "issue, inspect, rotate, commit, or revoke a Fleet credential", Commands: []*cli.Command{
		fleetCredentialIssue(), fleetCredentialInspect(), fleetCredentialRotate(), fleetCredentialCommit(), fleetCredentialRevoke(),
	}}
}

func fleetCredentialIssue() *cli.Command {
	return &cli.Command{Name: "issue", Usage: "issue one capability-scoped credential", Flags: []cli.Flag{
		&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true}, &cli.StringFlag{Name: "kind", Required: true, Usage: "observer, steer, or interrupt"}, &cli.StringFlag{Name: "subject-id"}, &cli.StringSliceFlag{Name: "ship-id"}, &cli.DurationFlag{Name: "ttl", Value: time.Hour}, &cli.StringFlag{Name: "output", Required: true},
	}, Action: func(ctx context.Context, c *cli.Command) error {
		r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
		if err != nil {
			return err
		}
		kind, ships := c.String("kind"), c.StringSlice("ship-id")
		var cf credentialFile
		var revoke func()
		switch kind {
		case "observer":
			cred, e := r.IssueObserver(ships)
			if e != nil {
				return e
			}
			cf = credentialFile{bootstrapSchema, kind, c.String("fleet-id"), cred.CredentialID, 0, "", ships, cred.Secret}
			revoke = func() { _ = r.RevokeObserver(cred.CredentialID) }
		case "steer", "interrupt":
			if c.String("subject-id") == "" {
				return errors.New("subject_id_required")
			}
			capability := fleetidentity.SteerTurnCapability
			if kind == "interrupt" {
				capability = fleetidentity.InterruptTurnCapability
			}
			cred, e := r.IssueOperatorCapability(c.String("subject-id"), ships, capability, c.Duration("ttl"))
			if e != nil {
				return e
			}
			cf = credentialFile{bootstrapSchema, kind, c.String("fleet-id"), cred.Record.CredentialID, cred.Record.CredentialGeneration, cred.Record.SubjectID, cred.Record.ShipIDs, cred.Secret}
			revoke = func() { _ = r.RevokeOperatorCredential(cred.Record.CredentialID, cred.Record.CredentialGeneration) }
		default:
			return errors.New("unsupported_credential_kind")
		}
		b, _ := json.Marshal(cf)
		if err := createSecretFile(c.String("output"), append(b, '\n')); err != nil {
			revoke()
			return err
		}
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": bootstrapSchema, "kind": cf.Kind, "fleet_id": cf.FleetID, "credential_id": cf.CredentialID, "generation": cf.Generation, "subject_id": cf.SubjectID, "ship_ids": cf.ShipIDs})
	}}
}

func fleetCredentialInspect() *cli.Command {
	return &cli.Command{Name: "inspect", Usage: "inspect non-secret credential metadata", Flags: []cli.Flag{&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true}, &cli.StringFlag{Name: "kind", Required: true}, &cli.StringFlag{Name: "credential-id"}, &cli.StringFlag{Name: "ship-id"}, &cli.Uint64Flag{Name: "generation"}}, Action: func(ctx context.Context, c *cli.Command) error {
		r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
		if err != nil {
			return err
		}
		switch c.String("kind") {
		case "ship":
			x, e := r.InspectShip(c.String("ship-id"))
			if e != nil {
				return e
			}
			return json.NewEncoder(c.Writer).Encode(x)
		case "observer":
			x, e := r.InspectObserver(c.String("credential-id"))
			if e != nil {
				return e
			}
			return json.NewEncoder(c.Writer).Encode(x)
		case "steer", "interrupt":
			x, e := r.InspectOperator(c.String("credential-id"), c.Uint64("generation"))
			if e != nil {
				return e
			}
			return json.NewEncoder(c.Writer).Encode(x)
		default:
			return errors.New("unsupported_credential_kind")
		}
	}}
}

func fleetCredentialRotate() *cli.Command {
	return &cli.Command{Name: "rotate", Usage: "issue a replacement ship or operator credential", Flags: []cli.Flag{&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true}, &cli.StringFlag{Name: "kind", Required: true}, &cli.StringFlag{Name: "ship-id"}, &cli.StringFlag{Name: "credential-id"}, &cli.Uint64Flag{Name: "generation"}, &cli.DurationFlag{Name: "overlap", Value: 5 * time.Minute}, &cli.DurationFlag{Name: "ttl", Value: time.Hour}, &cli.StringFlag{Name: "output", Required: true}}, Action: func(ctx context.Context, c *cli.Command) error {
		r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
		if err != nil {
			return err
		}
		var cf credentialFile
		var revoke func()
		switch c.String("kind") {
		case "ship":
			cred, e := r.IssueShipRotation(c.String("ship-id"), c.Duration("overlap"))
			if e != nil {
				return e
			}
			cf = credentialFile{bootstrapSchema, "ship", c.String("fleet-id"), cred.CredentialID, 0, "", []string{c.String("ship-id")}, cred.Secret}
			revoke = func() { _ = r.RevokeShipCredential(c.String("ship-id"), cred.CredentialID) }
		case "steer", "interrupt":
			cred, e := r.RotateOperator(c.String("credential-id"), c.Uint64("generation"), c.Duration("overlap"), c.Duration("ttl"))
			if e != nil {
				return e
			}
			cf = credentialFile{bootstrapSchema, c.String("kind"), c.String("fleet-id"), cred.Record.CredentialID, cred.Record.CredentialGeneration, cred.Record.SubjectID, cred.Record.ShipIDs, cred.Secret}
			revoke = func() { _ = r.RevokeOperatorCredential(cred.Record.CredentialID, cred.Record.CredentialGeneration) }
		default:
			return errors.New("unsupported_credential_kind")
		}
		b, _ := json.Marshal(cf)
		if err := createSecretFile(c.String("output"), append(b, '\n')); err != nil {
			revoke()
			return err
		}
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": bootstrapSchema, "kind": cf.Kind, "fleet_id": cf.FleetID, "credential_id": cf.CredentialID, "generation": cf.Generation})
	}}
}

func fleetCredentialCommit() *cli.Command {
	return &cli.Command{Name: "commit", Usage: "commit a proved credential rotation", Flags: []cli.Flag{&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true}, &cli.StringFlag{Name: "kind", Required: true}, &cli.StringFlag{Name: "ship-id"}, &cli.StringFlag{Name: "credential-id", Required: true}, &cli.Uint64Flag{Name: "generation"}}, Action: func(ctx context.Context, c *cli.Command) error {
		r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
		if err != nil {
			return err
		}
		switch c.String("kind") {
		case "ship":
			err = r.CommitShipRotation(c.String("ship-id"), c.String("credential-id"))
		case "steer", "interrupt":
			err = r.CommitOperatorRotation(c.String("credential-id"), c.Uint64("generation"))
		default:
			return errors.New("unsupported_credential_kind")
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": bootstrapSchema, "committed": true, "kind": c.String("kind"), "credential_id": c.String("credential-id")})
	}}
}

func fleetCredentialRevoke() *cli.Command {
	return &cli.Command{Name: "revoke", Usage: "revoke a ship, observer, or operator credential", Flags: []cli.Flag{&cli.StringFlag{Name: "authority-store", Required: true}, &cli.StringFlag{Name: "fleet-id", Required: true}, &cli.StringFlag{Name: "kind", Required: true}, &cli.StringFlag{Name: "ship-id"}, &cli.StringFlag{Name: "credential-id"}, &cli.Uint64Flag{Name: "generation"}}, Action: func(ctx context.Context, c *cli.Command) error {
		r, err := fleetidentity.OpenRegistry(c.String("authority-store"), c.String("fleet-id"), nil, nil)
		if err != nil {
			return err
		}
		switch c.String("kind") {
		case "ship":
			if c.String("ship-id") == "" {
				return errors.New("ship_id_required")
			}
			err = r.RevokeShipCredential(c.String("ship-id"), c.String("credential-id"))
		case "observer":
			err = r.RevokeObserver(c.String("credential-id"))
		case "steer", "interrupt":
			err = r.RevokeOperatorCredential(c.String("credential-id"), c.Uint64("generation"))
		default:
			return errors.New("unsupported_credential_kind")
		}
		if err != nil {
			return err
		}
		return json.NewEncoder(c.Writer).Encode(map[string]any{"schema_version": bootstrapSchema, "revoked": true, "kind": c.String("kind"), "credential_id": c.String("credential-id")})
	}}
}

func randomID(prefix string) (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func createSecretFile(path string, data []byte) error {
	if err := rejectRepositoryPath(path); err != nil {
		return err
	}
	abs, err := cleanAbsolute(path)
	if err != nil {
		return err
	}
	parent := filepath.Dir(abs)
	info, err := os.Lstat(parent)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe_secret_output")
	}
	if _, err := os.Lstat(abs); err == nil {
		return errors.New("secret_output_exists")
	} else if !os.IsNotExist(err) {
		return errors.New("unsafe_secret_output")
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("secret_output_failed")
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(abs)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return errors.New("secret_output_failed")
	}
	if _, err := f.Write(data); err != nil || f.Sync() != nil || f.Close() != nil {
		return errors.New("secret_output_failed")
	}
	ok = true
	return nil
}

func readProtectedFile(path string) ([]byte, error) {
	abs, err := cleanAbsolute(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(abs)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || info.Size() > 16<<10 {
		return nil, errors.New("unsafe_secret_input")
	}
	b, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func cleanAbsolute(path string) (string, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("absolute_path_required")
	}
	return path, nil
}

func rejectRepositoryPath(path string) error {
	abs, err := cleanAbsolute(path)
	if err != nil {
		return err
	}
	root, err := project.FindRoot(".")
	if err != nil {
		return nil
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return errors.New("repository_path_forbidden")
	}
	if rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))) {
		return errors.New("repository_path_forbidden")
	}
	return nil
}
