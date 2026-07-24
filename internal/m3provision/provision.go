//go:build unix

// Package m3provision implements the administrator-owned, fixed-profile M3
// prerequisite provisioner. It never starts Fleet, a service, or the
// qualifier; all writes are create-new and committed by one final rename.
package m3provision

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetconfig"
	"github.com/luthermonson/shipmates/internal/fleetidentity"
)

const (
	Profile             = "ubuntu-rojo-localhost"
	FleetID             = "flt_ubuntu_rojo_localhost"
	ServiceIdentity     = fleetconfig.ServiceIdentity
	FleetURL            = "wss://localhost:8443/api/fleet/v1/tunnel"
	DefaultBase         = "/etc/shipmates/m3-qualifier"
	DefaultHelper       = "/usr/libexec/shipmates/shipmates-cgroup-launcher"
	DefaultRunner       = "/usr/libexec/shipmates/shipmates-m3-qualifier-run"
	DefaultUnit         = "/etc/systemd/system/shipmates-m3-qualifier.service"
	LauncherVersion     = "shipmates-cgroup-launcher-v1"
	provisionSchema     = "shipmates.m3.provisioned.v1"
	commanderSchema     = "shipmates.m3.commander-credential.v1"
	shipMetadataSchema  = "shipmates.m3.ship-metadata.v1"
	stateMode           = 0600
	directoryMode       = 0700
	certificateValidity = 24 * time.Hour
)

type Layout struct {
	Base, Helper, Unit string
}

type Result struct {
	Schema      string `json:"schema"`
	Profile     string `json:"profile"`
	FleetID     string `json:"fleet_id"`
	ShipID      string `json:"ship_id"`
	ShipCredID  string `json:"ship_credential_id"`
	CommanderID string `json:"commander_credential_id"`
	TrustSHA256 string `json:"trust_sha256"`
	SPKISHA256  string `json:"tls_spki_sha256"`
	ConfigPath  string `json:"config_path"`
	ServicePath string `json:"service_path"`
	CaptainCmd  string `json:"captain_command"`
}

type DelegatedProbePlan struct {
	Schema              string   `json:"schema"`
	Root                string   `json:"delegated_root"`
	Controls            []string `json:"controls"`
	StartsQualification bool     `json:"starts_qualification"`
	RequiresCleanup     bool     `json:"requires_cleanup"`
}

type commanderFile struct {
	Schema       string    `json:"schema"`
	FleetID      string    `json:"fleet_id"`
	ShipID       string    `json:"ship_id"`
	CredentialID string    `json:"credential_id"`
	SubjectID    string    `json:"subject_id"`
	Capability   string    `json:"capability"`
	Generation   uint64    `json:"generation"`
	ShipIDs      []string  `json:"ship_ids"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	Revoked      bool      `json:"revoked"`
	Secret       string    `json:"secret"`
}

type shipProofFile struct {
	Schema       string `json:"schema"`
	FleetID      string `json:"fleet_id"`
	ShipID       string `json:"ship_id"`
	CredentialID string `json:"credential_id"`
	Secret       string `json:"secret"`
}

type shipMetadata struct {
	Schema           string `json:"schema"`
	FleetID          string `json:"fleet_id"`
	ShipID           string `json:"ship_id"`
	CredentialID     string `json:"credential_id"`
	CredentialSecret string `json:"credential_secret"`
	Destination      string `json:"destination"`
	Service          string `json:"service_identity"`
}

// ValidateInvocation is deliberately independent of filesystem writes so the
// command boundary can be exhaustively tested without root or host changes.
func ValidateInvocation(args []string, env []string, euid int) error {
	if euid != 0 {
		return errors.New("root_required")
	}
	if len(args) != 2 || args[0] != "--profile" || args[1] != Profile {
		return errors.New("fixed_profile_required")
	}
	for _, entry := range env {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		upper := strings.ToUpper(key)
		if strings.Contains(upper, "SECRET") || strings.Contains(upper, "TOKEN") || strings.Contains(upper, "PASSWORD") || strings.Contains(upper, "PRIVATE_KEY") {
			return errors.New("secret_environment_refused")
		}
	}
	return nil
}

// BuildDelegatedProbePlan is the provisioning-time seam. It records the
// disposable-child controls that the later administrator probe must exercise;
// it deliberately does not open cgroups, fork, dial, or start qualification.
func BuildDelegatedProbePlan(root string) (DelegatedProbePlan, error) {
	if !safeAbsolute(root) || root == "/" {
		return DelegatedProbePlan{}, errors.New("unsafe_delegated_root")
	}
	return DelegatedProbePlan{Schema: "shipmates.m3.delegated-probe.v1", Root: root, Controls: []string{"cgroup.procs", "cgroup.kill", "cgroup.events", "pidfd", "populated-zero", "leaf-removal"}, StartsQualification: false, RequiresCleanup: true}, nil
}

func ValidateCredentialUnit(unit string, encrypted bool) error {
	plain, enc := 0, 0
	seen := map[string]bool{}
	for _, line := range strings.Split(unit, "\n") {
		if strings.HasPrefix(line, "LoadCredential=") {
			plain++
			name := strings.TrimPrefix(line, "LoadCredential=")
			if !strings.HasPrefix(name, "ship.json:") && !strings.HasPrefix(name, "commander.json:") {
				return errors.New("credential_mapping_invalid")
			}
			key := strings.SplitN(name, ":", 2)[0]
			if seen[key] {
				return errors.New("credential_mapping_duplicate")
			}
			seen[key] = true
		}
		if strings.HasPrefix(line, "LoadCredentialEncrypted=") {
			enc++
			name := strings.TrimPrefix(line, "LoadCredentialEncrypted=")
			if !strings.HasPrefix(name, "ship.json:") && !strings.HasPrefix(name, "commander.json:") {
				return errors.New("credential_mapping_invalid")
			}
			key := strings.SplitN(name, ":", 2)[0]
			if seen[key] {
				return errors.New("credential_mapping_duplicate")
			}
			seen[key] = true
		}
	}
	if encrypted {
		if enc != 2 || plain != 0 {
			return errors.New("credential_encryption_downgrade")
		}
	} else if plain != 2 || enc != 0 {
		return errors.New("credential_mapping_count")
	}
	if !seen["ship.json"] || !seen["commander.json"] {
		return errors.New("credential_mapping_incomplete")
	}
	return nil
}

// ProvisionAt provisions only the supplied layout. Production calls it with
// /etc paths; tests use an isolated temporary parent and never need root.
func ProvisionAt(layout Layout) (Result, error) {
	if !safeAbsolute(layout.Base) || !safeAbsolute(layout.Helper) || !safeAbsolute(layout.Unit) {
		return Result{}, errors.New("unsafe_layout")
	}
	parent := filepath.Dir(layout.Base)
	if _, err := os.Stat(parent); err != nil {
		return Result{}, errors.New("provision_parent_unavailable")
	}
	if _, err := os.Lstat(layout.Base); err == nil {
		return Result{}, errors.New("provision_target_exists")
	} else if !os.IsNotExist(err) {
		return Result{}, errors.New("provision_target_ambiguous")
	}
	if _, err := os.Stat(layout.Helper); err != nil {
		return Result{}, errors.New("launcher_unavailable")
	}
	if _, err := os.Lstat(layout.Unit); err == nil {
		return Result{}, errors.New("service_target_exists")
	} else if !os.IsNotExist(err) {
		return Result{}, errors.New("service_target_ambiguous")
	}
	stage, err := os.MkdirTemp(parent, ".m3-qualifier-stage-")
	if err != nil {
		return Result{}, errors.New("provision_stage_unavailable")
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0755); err != nil {
		return Result{}, errors.New("provision_stage_unavailable")
	}
	for _, p := range []string{"trust", "secrets", "state", "tls", "authority", "helper"} {
		if err := os.Mkdir(filepath.Join(stage, p), directoryMode); err != nil {
			return Result{}, errors.New("provision_directory_failed")
		}
	}
	for _, p := range []string{"trust", "tls", "helper"} {
		if err := os.Chmod(filepath.Join(stage, p), 0755); err != nil {
			return Result{}, errors.New("provision_directory_failed")
		}
	}
	if err := os.Mkdir(filepath.Join(stage, "credentials"), directoryMode); err != nil {
		return Result{}, errors.New("provision_directory_failed")
	}

	registry, err := fleetidentity.OpenRegistry(filepath.Join(stage, "authority"), FleetID, nil, nil)
	if err != nil {
		return Result{}, errors.New("authority_open_failed")
	}
	artifact, err := registry.CreateEnrollment(time.Hour)
	if err != nil {
		return Result{}, errors.New("enrollment_create_failed")
	}
	enrolled, err := registry.Enroll(artifact.ArtifactID, artifact.Secret, "txn_ubuntu_rojo_localhost_0001")
	clearString(&artifact.Secret)
	if err != nil {
		return Result{}, errors.New("enrollment_failed")
	}
	active, err := registry.IssueCommander("cmdr_ubuntu_rojo_localhost", []string{enrolled.ShipID}, time.Hour)
	if err != nil {
		return Result{}, errors.New("commander_issue_failed")
	}
	if err := proveDisposableCommander(registry, enrolled.ShipID); err != nil {
		return Result{}, err
	}

	caCert, caKey, leafCert, leafKey, trust, spki, err := makeTLS()
	if err != nil {
		return Result{}, errors.New("tls_generation_failed")
	}
	if err := createFile(filepath.Join(stage, "tls", "ca.pem"), caCert, 0600); err != nil {
		return Result{}, err
	}
	if err := createFile(filepath.Join(stage, "tls", "ca-key.pem"), caKey, 0600); err != nil {
		return Result{}, err
	}
	if err := createFile(filepath.Join(stage, "tls", "server.pem"), leafCert, 0600); err != nil {
		return Result{}, err
	}
	if err := createFile(filepath.Join(stage, "tls", "server-key.pem"), leafKey, 0600); err != nil {
		return Result{}, err
	}
	if err := createFile(filepath.Join(stage, "trust", "fleet-ca.pem"), trust, 0644); err != nil {
		return Result{}, err
	}

	shipState := fleetidentity.ShipState{SchemaVersion: fleetidentity.ShipStateSchema, FleetID: FleetID, ShipID: enrolled.ShipID, FleetDestination: FleetURL, FleetServiceIdentity: ServiceIdentity, CredentialID: enrolled.Credential.CredentialID, CredentialSecret: enrolled.Credential.Secret}
	if err := fleetidentity.StoreShipState(filepath.Join(stage, "state", "ship"), shipState); err != nil {
		return Result{}, errors.New("ship_state_failed")
	}
	if err := writeJSON(filepath.Join(stage, "credentials", "ship.json"), shipMetadata{shipMetadataSchema, FleetID, enrolled.ShipID, enrolled.Credential.CredentialID, enrolled.Credential.Secret, FleetURL, ServiceIdentity}, stateMode); err != nil {
		return Result{}, err
	}
	shipProof := shipProofFile{fleetconfig.CredentialSchema, FleetID, enrolled.ShipID, enrolled.Credential.CredentialID, enrolled.Credential.Secret}
	if err := writeJSON(filepath.Join(stage, "secrets", "ship-proof.json"), shipProof, stateMode); err != nil {
		return Result{}, err
	}
	if err := writeJSON(filepath.Join(stage, "credentials", "commander.json"), commanderFile{Schema: commanderSchema, FleetID: FleetID, ShipID: enrolled.ShipID, CredentialID: active.Record.CredentialID, SubjectID: active.Record.SubjectID, Capability: active.Record.Capability, Generation: active.Record.CredentialGeneration, ShipIDs: active.Record.ShipIDs, IssuedAt: active.Record.IssuedAt, ExpiresAt: active.Record.ExpiresAt, Revoked: active.Record.Revoked, Secret: active.Secret}, stateMode); err != nil {
		return Result{}, err
	}
	config := fleetconfig.Config{Schema: fleetconfig.Schema, FleetID: FleetID, ShipID: enrolled.ShipID, CredentialID: enrolled.Credential.CredentialID, FleetURL: FleetURL, FleetDNS: "localhost", TLSServerName: "localhost", TrustFile: "fleet-ca.pem", TrustSHA256: hash(trust), SPKISHA256: hex.EncodeToString(spki), CredentialFile: "ship-proof.json", Service: ServiceIdentity, M7: fleetconfig.M7Identity, M3: fleetconfig.M3Identity}
	if err := writeJSON(filepath.Join(stage, "fleet.json"), config, 0644); err != nil {
		return Result{}, err
	}
	clearString(&enrolled.Credential.Secret)
	clearString(&active.Secret)
	manifest, err := helperManifest(layout.Helper)
	if err != nil {
		return Result{}, err
	}
	manifest["path"] = DefaultHelper
	if err := writeJSON(filepath.Join(stage, "helper-manifest.json"), manifest, stateMode); err != nil {
		return Result{}, err
	}
	helperBytes, err := os.ReadFile(layout.Helper)
	if err != nil || createFile(filepath.Join(stage, "helper", "shipmates-cgroup-launcher"), helperBytes, 0755) != nil {
		return Result{}, errors.New("launcher_stage_failed")
	}
	if err := writeJSON(filepath.Join(stage, "state", "provisioned.json"), Result{Schema: provisionSchema, Profile: Profile, FleetID: FleetID, ShipID: enrolled.ShipID, ShipCredID: enrolled.Credential.CredentialID, CommanderID: active.Record.CredentialID, TrustSHA256: hash(trust), SPKISHA256: hex.EncodeToString(spki)}, stateMode); err != nil {
		return Result{}, err
	}
	probePlan, err := BuildDelegatedProbePlan("/sys/fs/cgroup/shipmates")
	if err != nil || writeJSON(filepath.Join(stage, "state", "delegated-probe.json"), probePlan, stateMode) != nil {
		return Result{}, errors.New("delegated_probe_plan_failed")
	}
	if err := fleetconfigLoad(stage); err != nil {
		return Result{}, err
	}
	if err := os.Rename(stage, layout.Base); err != nil {
		return Result{}, errors.New("provision_commit_failed")
	}
	unitBytes := []byte(unitText(layout.Base, layout.Helper))
	if err := ValidateCredentialUnit(string(unitBytes), false); err != nil || createFile(layout.Unit, unitBytes, 0600) != nil {
		return Result{}, errors.New("service_commit_failed")
	}
	if err := syncDir(parent); err != nil {
		return Result{}, errors.New("provision_commit_uncertain")
	}
	return Result{Schema: provisionSchema, Profile: Profile, FleetID: FleetID, ShipID: enrolled.ShipID, ShipCredID: enrolled.Credential.CredentialID, CommanderID: active.Record.CredentialID, TrustSHA256: hash(trust), SPKISHA256: hex.EncodeToString(spki), ConfigPath: filepath.Join(layout.Base, "fleet.json"), ServicePath: layout.Unit, CaptainCmd: "systemctl start shipmates-m3-qualifier.service"}, nil
}

func safeAbsolute(path string) bool {
	return path != "" && filepath.IsAbs(path) && filepath.Clean(path) == path && !strings.Contains(path, "//")
}

func fleetconfigLoad(stage string) error {
	if _, err := fleetconfig.LoadRuntimeProfileAt(filepath.Join(stage, "fleet.json"), filepath.Join(stage, "trust"), filepath.Join(stage, "credentials", "ship.json"), filepath.Join(stage, "credentials", "commander.json"), true); err != nil {
		return errors.New("protected_config_validation_failed")
	}
	return nil
}

func proveDisposableCommander(r *fleetidentity.Registry, shipID string) error {
	d, err := r.IssueCommander("cmdr_disposable_ubuntu_rojo", []string{shipID}, time.Hour)
	if err != nil {
		return errors.New("disposable_issue_failed")
	}
	n, err := r.RotateCommander(d.Record.CredentialID, d.Record.CredentialGeneration, time.Minute, time.Hour)
	if err != nil {
		clearString(&d.Secret)
		return errors.New("disposable_rotate_failed")
	}
	if err := r.CommitCommanderRotation(n.Record.CredentialID, n.Record.CredentialGeneration); err != nil {
		clearString(&d.Secret)
		clearString(&n.Secret)
		return errors.New("disposable_commit_failed")
	}
	if _, err := r.AuthenticateCommander(d.Record.CredentialID, d.Secret, FleetID, shipID); err == nil {
		clearString(&d.Secret)
		clearString(&n.Secret)
		return errors.New("disposable_old_credential_active")
	}
	clearString(&d.Secret)
	if _, err := r.AuthenticateCommander(n.Record.CredentialID, n.Secret, FleetID, shipID); err != nil {
		clearString(&n.Secret)
		return errors.New("disposable_proof_failed")
	}
	if err := r.RevokeCommanderCredential(n.Record.CredentialID, n.Record.CredentialGeneration); err != nil {
		clearString(&n.Secret)
		return errors.New("disposable_revoke_failed")
	}
	clearString(&n.Secret)
	return nil
}

func makeTLS() ([]byte, []byte, []byte, []byte, []byte, []byte, error) {
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	serial, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	caTemplate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "Shipmates UbuntuRojo M3 local CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(certificateValidity), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	leafTemplate := &x509.Certificate{SerialNumber: mustSerial(), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(certificateValidity), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		return nil, nil, nil, nil, nil, nil, err
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	caKeyDER, _ := x509.MarshalPKCS8PrivateKey(caKey)
	leafKeyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER})
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER})
	cert, _ := x509.ParseCertificate(leafDER)
	spki := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return caPEM, caKeyPEM, leafPEM, leafKeyPEM, caPEM, spki[:], nil
}

func mustSerial() *big.Int {
	n, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	return n
}
func hash(b []byte) string { x := sha256.Sum256(b); return hex.EncodeToString(x[:]) }
func clearString(s *string) {
	if s != nil {
		b := []byte(*s)
		for i := range b {
			b[i] = 0
		}
		*s = ""
	}
}

func helperManifest(path string) (map[string]string, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode()&0022 != 0 || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("launcher_unavailable")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("launcher_unavailable")
	}
	if len(b) < 4 || string(b[:4]) != "\x7fELF" {
		return nil, errors.New("launcher_not_elf")
	}
	x := sha256.Sum256(b)
	return map[string]string{"schema": "shipmates.m3.helper-manifest.v1", "version": LauncherVersion, "path": path, "sha256": hex.EncodeToString(x[:])}, nil
}

func unitText(base, helper string) string {
	return "[Unit]\nDescription=Shipmates M3 localhost Fleet prerequisite (not started by provisioner)\nAfter=network.target\n\n[Service]\nType=simple\nUser=shipmates\nGroup=shipmates\nDelegate=yes\nLoadCredential=ship.json:" + filepath.Join(base, "credentials", "ship.json") + "\nLoadCredential=commander.json:" + filepath.Join(base, "credentials", "commander.json") + "\nExecStart=" + DefaultRunner + "\nNoNewPrivileges=yes\nPrivateTmp=yes\nProtectSystem=strict\nProtectHome=yes\nPrivateDevices=yes\nRestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX\nUMask=0077\nMemoryMax=512M\nTasksMax=256\nCPUQuota=100%\nTimeoutStartSec=10s\nTimeoutStopSec=10s\nReadOnlyPaths=" + filepath.Join(base, "fleet.json") + " " + filepath.Join(base, "trust") + " " + filepath.Join(base, "tls") + " " + filepath.Join(base, "helper") + "\nReadWritePaths=" + filepath.Join(base, "authority") + " " + filepath.Join(base, "state") + "\n\n[Install]\nWantedBy=multi-user.target\n# launcher=" + helper + "\n"
}

func writeJSON(path string, v any, mode os.FileMode) error {
	b, err := json.Marshal(v)
	if err != nil {
		return errors.New("json_encode_failed")
	}
	return createFile(path, append(b, '\n'), mode)
}
func createFile(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return errors.New("protected_write_failed")
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(path)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return errors.New("protected_write_failed")
	}
	if err := f.Sync(); err != nil {
		return errors.New("protected_sync_failed")
	}
	if err := f.Close(); err != nil {
		return errors.New("protected_close_failed")
	}
	ok = true
	return nil
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
