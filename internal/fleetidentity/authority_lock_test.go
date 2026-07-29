//go:build unix

package fleetidentity

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// openAuthorityPair returns two independently opened registries over one store,
// modelling the long-running `fleet serve-observer` process and a `fleet
// credential ...` CLI invocation, plus one enrolled ship.
func openAuthorityPair(t *testing.T) (*Registry, *Registry, string, EnrollmentResult) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "authority")
	server, err := OpenRegistry(dir, "fleet-000000000001", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := server.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := server.Enroll(artifact.ArtifactID, artifact.Secret, "txn-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	cli, err := OpenRegistry(dir, "fleet-000000000001", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return server, cli, dir, ship
}

// TestCLIRevocationReachesTheRunningServer is the first half of the missing
// cross-process coordination: the server held its own in-memory snapshot and
// never observed a revocation committed by another process.
func TestCLIRevocationReachesTheRunningServer(t *testing.T) {
	server, cli, _, ship := openAuthorityPair(t)
	shipID := ship.ShipID
	operator, err := server.IssueOperator("operator-00000001", []string{shipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, generation := operator.Record.CredentialID, operator.Record.CredentialGeneration
	if _, err := server.AuthenticateOperator(id, operator.Secret, operator.Record.FleetID, shipID); err != nil {
		t.Fatalf("live credential refused by the server: %v", err)
	}
	if err := cli.RevokeOperatorCredential(id, generation); err != nil {
		t.Fatal(err)
	}
	if _, err := server.AuthenticateOperator(id, operator.Secret, operator.Record.FleetID, shipID); !IsCode(err, Unauthorized) {
		t.Fatalf("the server authenticated a credential another process revoked: %v", err)
	}
	record, err := server.InspectOperator(id, generation)
	if err != nil || !record.Revoked {
		t.Fatalf("the server did not observe the revocation: record=%+v err=%v", record, err)
	}
}

// TestServerCommitDoesNotResurrectARevokedCredential is the second half: the
// server's next commit rewrote the whole file from its stale snapshot, so a
// handshake un-revoked the credential on disk.
func TestServerCommitDoesNotResurrectARevokedCredential(t *testing.T) {
	server, cli, dir, ship := openAuthorityPair(t)
	shipID := ship.ShipID
	operator, err := server.IssueOperator("operator-00000001", []string{shipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	id, generation := operator.Record.CredentialID, operator.Record.CredentialGeneration
	// The server has the credential in memory and has not yet seen the revoke.
	if _, err := server.AuthenticateOperator(id, operator.Secret, operator.Record.FleetID, shipID); err != nil {
		t.Fatal(err)
	}
	if err := cli.RevokeOperatorCredential(id, generation); err != nil {
		t.Fatal(err)
	}
	// A ship handshake is the server's most frequent commit.
	principal, err := server.AuthenticateShip(ship.Credential.CredentialID, ship.Credential.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.AllocateConnectionGeneration(principal); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenRegistry(dir, "fleet-000000000001", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	record, err := reopened.InspectOperator(id, generation)
	if err != nil || !record.Revoked {
		t.Fatalf("a server commit un-revoked the credential on disk: record=%+v err=%v", record, err)
	}
}

// TestConcurrentAuthorityWritersDoNotCorruptTheStore exercises the file lock and
// the unique temporary name together. Two writers under one fixed temp name
// could unlink and rename each other's half-written file into place.
func TestConcurrentAuthorityWritersDoNotCorruptTheStore(t *testing.T) {
	_, _, dir, ship := openAuthorityPair(t)
	shipID := ship.ShipID
	const writers, rounds = 6, 12
	registries := make([]*Registry, writers)
	for i := range registries {
		r, err := OpenRegistry(dir, "fleet-000000000001", nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		registries[i] = r
	}
	errs := make(chan error, writers*rounds)
	var wg sync.WaitGroup
	for i := range registries {
		wg.Add(1)
		go func(r *Registry) {
			defer wg.Done()
			for round := 0; round < rounds; round++ {
				principal, err := r.AuthenticateShip(ship.Credential.CredentialID, ship.Credential.Secret)
				if err != nil {
					errs <- err
					return
				}
				if _, err := r.AllocateConnectionGeneration(principal); err != nil {
					errs <- err
					return
				}
			}
		}(registries[i])
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent authority commit failed: %v", err)
	}
	reopened, err := OpenRegistry(dir, "fleet-000000000001", nil, nil)
	if err != nil {
		t.Fatalf("the store did not survive concurrent writers: %v", err)
	}
	// Every commit was serialized, so no generation was lost or duplicated.
	if got := reopened.ConnectionGeneration(shipID); got != writers*rounds {
		t.Fatalf("connection generation=%d want %d", got, writers*rounds)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Fatalf("a temporary authority file was left behind: %s", entry.Name())
		}
	}
}

// TestAuthorityTempNamesAreUnique guards the fixed-name temp file that made
// unlink-then-O_EXCL a race between writers rather than a barrier.
func TestAuthorityTempNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		name, err := uniqueTempName(".fleet-authority.")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(name, ".fleet-authority.") || !strings.HasSuffix(name, ".tmp") {
			t.Fatalf("temp name=%q", name)
		}
		if seen[name] {
			t.Fatalf("temp name %q was reused", name)
		}
		seen[name] = true
	}
}
