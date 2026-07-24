//go:build unix

package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
)

func runFleetBootstrap(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := Fleet()
	cmd.Writer = &out
	cmd.ErrWriter = &out
	err := cmd.Run(context.Background(), append([]string{"fleet"}, args...))
	return out.String(), err
}

func TestFleetBootstrapPublicFlowKeepsSecretsOutOfOutput(t *testing.T) {
	root := t.TempDir()
	authority := filepath.Join(root, "authority")
	artifact := filepath.Join(root, "enrollment.json")
	identity := filepath.Join(root, "ship-state")
	if _, err := runFleetBootstrap(t, "init", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	if out, err := runFleetBootstrap(t, "enrollment", "create", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--output", artifact); err != nil {
		t.Fatal(err)
	} else if strings.Contains(out, "secret") {
		t.Fatalf("enrollment output names secret: %s", out)
	}
	info, err := os.Stat(artifact)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("artifact mode/stat = %v %v", info, err)
	}
	var artifactDoc enrollmentFile
	b, err := os.ReadFile(artifact)
	if err != nil || json.Unmarshal(b, &artifactDoc) != nil || artifactDoc.Secret == "" {
		t.Fatalf("artifact did not contain protected enrollment material: %v", err)
	}
	out, err := runFleetBootstrap(t, "enrollment", "consume", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--enrollment-file", artifact, "--identity-store", identity, "--fleet-destination", "https://fleet.example", "--fleet-service-identity", "fleet-service")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, artifactDoc.Secret) || strings.Contains(out, "secret") {
		t.Fatalf("enrollment response exposed secret: %s", out)
	}
	if _, err := os.Stat(artifact); !os.IsNotExist(err) {
		t.Fatalf("one-use artifact remains: %v", err)
	}
	var state fleetidentity.ShipState
	if b, err := os.ReadFile(filepath.Join(identity, "identity.json")); err != nil || json.Unmarshal(b, &state) != nil || state.CredentialSecret == "" {
		t.Fatalf("ship state missing protected credential: %v", err)
	}
	shipCredential := filepath.Join(root, "observer.json")
	out, err = runFleetBootstrap(t, "credential", "issue", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--kind", "observer", "--ship-id", state.ShipID, "--output", shipCredential)
	if err != nil || strings.Contains(out, "secret") {
		t.Fatalf("observer issue output = %q err=%v", out, err)
	}
	if info, err := os.Stat(shipCredential); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode/stat = %v %v", info, err)
	}
}

func TestFleetBootstrapRejectsUnsafeSecretOutputs(t *testing.T) {
	root := t.TempDir()
	authority := filepath.Join(root, "authority")
	if _, err := runFleetBootstrap(t, "init", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	// rejectRepositoryPath resolves the project via FindRoot from the
	// process working directory, so establish a real shipmates project
	// there. (Previously this leaned on os.Getwd() — the package source
	// dir — being inside a shipmates project, which is only true when a
	// shipmates.yaml happens to exist above the checkout.)
	projectDir := t.TempDir()
	t.Chdir(projectDir)
	if err := os.WriteFile("shipmates.yaml", []byte("crew: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(wd, ".fleet-secret-test")
	if _, err := runFleetBootstrap(t, "enrollment", "create", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--output", inside); err == nil {
		t.Fatal("repository-relative output unexpectedly accepted")
	}
	if _, err := os.Lstat(inside); !os.IsNotExist(err) {
		t.Fatalf("rejected output was still created: %v", err)
	}
	link := filepath.Join(root, "link")
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := runFleetBootstrap(t, "enrollment", "create", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--output", link); err == nil {
		t.Fatal("symlink output unexpectedly accepted")
	}
}

func TestFleetBootstrapPublicTwoShipIsolationAndCapabilityScopes(t *testing.T) {
	root := t.TempDir()
	authority := filepath.Join(root, "authority")
	if _, err := runFleetBootstrap(t, "init", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	type shipFixture struct {
		artifact, identity string
		state              fleetidentity.ShipState
	}
	ships := make([]shipFixture, 2)
	for i := range ships {
		ships[i].artifact = filepath.Join(root, "enrollment-"+string(rune('a'+i)))
		ships[i].identity = filepath.Join(root, "ship-"+string(rune('a'+i)))
		if _, err := runFleetBootstrap(t, "enrollment", "create", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--output", ships[i].artifact); err != nil {
			t.Fatal(err)
		}
		if _, err := runFleetBootstrap(t, "enrollment", "consume", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--enrollment-file", ships[i].artifact, "--identity-store", ships[i].identity, "--fleet-destination", "https://fleet.example", "--fleet-service-identity", "fleet-service"); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(ships[i].identity, "identity.json"))
		if err != nil || json.Unmarshal(b, &ships[i].state) != nil {
			t.Fatalf("ship %d state: %v", i, err)
		}
	}
	if ships[0].state.ShipID == ships[1].state.ShipID || ships[0].state.CredentialID == ships[1].state.CredentialID {
		t.Fatalf("two public enrollments shared identity: %#v %#v", ships[0].state, ships[1].state)
	}
	issue := func(kind, subject, ship string) {
		t.Helper()
		output := filepath.Join(root, kind+"-credential")
		args := []string{"credential", "issue", "--authority-store", authority, "--fleet-id", "flt_0123456789abcdef", "--kind", kind, "--subject-id", subject, "--ship-id", ship, "--output", output}
		out, err := runFleetBootstrap(t, args...)
		if err != nil {
			t.Fatal(err)
		}
		var meta struct {
			CredentialID string `json:"credential_id"`
			Generation   uint64 `json:"generation"`
		}
		if err := json.Unmarshal([]byte(out), &meta); err != nil {
			t.Fatal(err)
		}
		r, err := fleetidentity.OpenRegistry(authority, "flt_0123456789abcdef", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		cred, err := r.InspectOperator(meta.CredentialID, meta.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if len(cred.ShipIDs) != 1 || cred.ShipIDs[0] != ship {
			t.Fatalf("%s allowlist = %#v", kind, cred.ShipIDs)
		}
	}
	issue("steer", "operator-steer-0001", ships[0].state.ShipID)
	issue("interrupt", "operator-interrupt-0001", ships[1].state.ShipID)
}
