package fleetconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAtValidatesClosedBindingsAndKeepsSecretInProofSeam(t *testing.T) {
	root, err := os.MkdirTemp(".", ".fleetconfig-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	trustRoot := filepath.Join(root, "trust")
	secretRoot := filepath.Join(root, "secrets")
	configRoot := filepath.Join(root, "etc", "shipmates", "m3-qualifier")
	for _, dir := range []string{trustRoot, secretRoot, configRoot} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	trust := []byte("pinned trust anchor\n")
	trustHash := sha256.Sum256(trust)
	if err := os.WriteFile(filepath.Join(trustRoot, "fleet-ca.pem"), trust, 0600); err != nil {
		t.Fatal(err)
	}
	secret := strings.Repeat("s", 24)
	cred := map[string]string{"schema": CredentialSchema, "fleet_id": "flt_0123456789abcdef", "ship_id": "shp_0123456789abcdef", "credential_id": "cred_0123456789abcdef", "secret": secret}
	credBytes, _ := json.Marshal(cred)
	if err := os.WriteFile(filepath.Join(secretRoot, "commander.json"), credBytes, 0600); err != nil {
		t.Fatal(err)
	}
	c := Config{Schema: Schema, FleetID: cred["fleet_id"], ShipID: cred["ship_id"], CredentialID: cred["credential_id"], FleetURL: "wss://fleet.example.test:8443/api/fleet/v1/tunnel", FleetDNS: "fleet.example.test", TLSServerName: "fleet.example.test", TrustFile: "fleet-ca.pem", TrustSHA256: hex.EncodeToString(trustHash[:]), SPKISHA256: strings.Repeat("b", 64), CredentialFile: "commander.json", Service: ServiceIdentity, M7: M7Identity, M3: M3Identity}
	configBytes, _ := json.Marshal(c)
	configPath := filepath.Join(configRoot, "fleet.json")
	if err := os.WriteFile(configPath, configBytes, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadAt(configPath, trustRoot, secretRoot)
	if err != nil {
		t.Fatal(err)
	}
	if string(loaded.Trust) != string(trust) || strings.Contains(string(configBytes), secret) {
		t.Fatal("secret crossed public config boundary")
	}
	called := false
	if err := loaded.UseForShipProof(func(fleet, ship, credential string, got []byte) error {
		called = true
		if fleet != c.FleetID || ship != c.ShipID || credential != c.CredentialID || string(got) != secret {
			t.Fatal("incorrect proof binding")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("proof seam was not called")
	}
	if err := loaded.UseForShipProof(func(string, string, string, []byte) error { return nil }); err == nil {
		t.Fatal("consumed credential was reusable")
	}
	// LoadAt is deliberately the non-production fixture seam. An invoking
	// account must not be able to provision the production qualifier binding.
	if _, err := LoadProtectedAt(configPath, trustRoot, secretRoot); err == nil {
		t.Fatal("current-user fixture accepted by production loader")
	}
}

func TestLoadRejectsDuplicateUnknownTrailingAndCredentialMismatch(t *testing.T) {
	root, err := os.MkdirTemp(".", ".fleetconfig-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	config := filepath.Join(root, "fleet.json")
	for _, raw := range []string{`{"schema":"x","schema":"y"}`, `{"schema":"x","unknown":1}`, `{"schema":"x"} {}`} {
		if err := os.WriteFile(config, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAt(config, root, root); err == nil {
			t.Fatalf("malformed config accepted: %s", raw)
		}
	}
}

func TestConfigGrammarRejectsNonCanonicalBindings(t *testing.T) {
	c := Config{Schema: Schema, FleetID: "bad id", ShipID: "shp_0123456789abcdef", CredentialID: "cred_0123456789abcdef", FleetURL: "http://fleet.example.test", FleetDNS: "fleet.example.test", TLSServerName: "fleet.example.test", TrustFile: "ca.pem", TrustSHA256: strings.Repeat("a", 64), SPKISHA256: strings.Repeat("b", 64), CredentialFile: "cred.json", Service: ServiceIdentity, M7: M7Identity, M3: M3Identity}
	if err := c.Validate(); err == nil {
		t.Fatal("noncanonical binding accepted")
	}
}

func TestConfigRequiresTheProductionWSSPathAndSPKIPin(t *testing.T) {
	base := Config{Schema: Schema, FleetID: "flt_0123456789abcdef", ShipID: "shp_0123456789abcdef", CredentialID: "cred_0123456789abcdef", FleetURL: "wss://fleet.example.test/api/fleet/v1/tunnel", FleetDNS: "fleet.example.test", TLSServerName: "fleet.example.test", TrustFile: "ca.pem", TrustSHA256: strings.Repeat("a", 64), SPKISHA256: strings.Repeat("b", 64), CredentialFile: "cred.json", Service: ServiceIdentity, M7: M7Identity, M3: M3Identity}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"https://fleet.example.test/api/fleet/v1/tunnel", "wss://fleet.example.test/other", "wss://fleet.example.test/api/fleet/v1/tunnel?redirect=1", "wss://other.example.test/api/fleet/v1/tunnel", "wss://fleet.example.test:0443/api/fleet/v1/tunnel", "wss://fleet.example.test:70000/api/fleet/v1/tunnel"} {
		c := base
		c.FleetURL = path
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted non-production endpoint %q", path)
		}
	}
	for _, pin := range []string{"", strings.Repeat("A", 64), strings.Repeat("c", 63)} {
		c := base
		c.SPKISHA256 = pin
		if err := c.Validate(); err == nil {
			t.Fatalf("accepted invalid SPKI pin %q", pin)
		}
	}
}
