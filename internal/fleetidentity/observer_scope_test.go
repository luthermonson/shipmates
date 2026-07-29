package fleetidentity

import (
	"testing"
	"time"
)

type fixedClock struct{ at time.Time }

func (c *fixedClock) Now() time.Time { return c.at }

type seededReader struct{ n byte }

func (r *seededReader) Read(p []byte) (int, error) {
	for i := range p {
		r.n++
		p[i] = r.n
	}
	return len(p), nil
}

func observerFixture(t *testing.T) (*Registry, *fixedClock, string) {
	t.Helper()
	clock := &fixedClock{at: time.Unix(1_000_000, 0)}
	r, err := NewRegistry("fleet-000000000001", clock, &seededReader{})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	ship, err := r.Enroll(artifact.ArtifactID, artifact.Secret, "txn-000000000001")
	if err != nil {
		t.Fatal(err)
	}
	return r, clock, ship.ShipID
}

// TestIssueObserverRefusesEmptyScope is the regression test for the credential
// that `fleet credential issue --kind observer` minted with no --ship-id. It was
// accepted, and the observer read path treated an empty scope as every ship.
func TestIssueObserverRefusesEmptyScope(t *testing.T) {
	r, _, shipID := observerFixture(t)
	for name, ships := range map[string][]string{"nil": nil, "empty": {}} {
		if _, err := r.IssueObserver(ships); !IsCode(err, InvalidInput) {
			t.Fatalf("%s ship scope minted a fleet-wide observer: %v", name, err)
		}
	}
	if _, err := r.IssueObserver([]string{shipID}); err != nil {
		t.Fatalf("exact ship scope refused: %v", err)
	}
}

// TestObserverCredentialIsTTLBounded covers the missing expiry: operator and
// commander credentials were TTL bounded and observers were issued forever.
func TestObserverCredentialIsTTLBounded(t *testing.T) {
	r, clock, shipID := observerFixture(t)
	issued := clock.at
	credential, err := r.IssueObserver([]string{shipID})
	if err != nil {
		t.Fatal(err)
	}
	record, err := r.InspectObserver(credential.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	if !record.IssuedAt.Equal(issued) || !record.ExpiresAt.Equal(issued.Add(ObserverTTL)) {
		t.Fatalf("observer record is not TTL bounded: %+v", record)
	}
	if _, err := r.AuthenticateObserver(credential.CredentialID, credential.Secret); err != nil {
		t.Fatalf("live observer refused: %v", err)
	}
	clock.at = record.ExpiresAt
	if _, err := r.AuthenticateObserver(credential.CredentialID, credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("expired observer authenticated: %v", err)
	}
	expired, err := r.InspectObserver(credential.CredentialID)
	if err != nil || !expired.Revoked {
		t.Fatalf("expiry was not made durable: record=%+v err=%v", expired, err)
	}
	for name, ttl := range map[string]time.Duration{"zero": 0, "too-short": time.Second, "too-long": 48 * time.Hour} {
		if _, err := r.IssueObserverTTL([]string{shipID}, ttl); !IsCode(err, InvalidInput) {
			t.Fatalf("%s observer ttl accepted: %v", name, err)
		}
	}
}

// TestRestoredObserverWithoutExpiryFailsClosed covers a store written before the
// TTL requirement: such a record must not survive as an unbounded credential.
func TestRestoredObserverWithoutExpiryFailsClosed(t *testing.T) {
	r, _, shipID := observerFixture(t)
	credential, err := r.IssueObserver([]string{shipID})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := r.durableLocked()
	for i := range snapshot.Observers {
		snapshot.Observers[i].Issued, snapshot.Observers[i].Expires = time.Time{}, time.Time{}
	}
	if err := r.restoreLocked(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AuthenticateObserver(credential.CredentialID, credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("an observer restored with no expiry still authenticated: %v", err)
	}
}

// TestAuthenticatedObserverScopeIsNeverEmpty guards the property the read path
// now depends on: a principal handed to fleetobserver always names its ships.
func TestAuthenticatedObserverScopeIsNeverEmpty(t *testing.T) {
	r, _, shipID := observerFixture(t)
	credential, err := r.IssueObserver([]string{shipID})
	if err != nil {
		t.Fatal(err)
	}
	principal, err := r.AuthenticateObserver(credential.CredentialID, credential.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if len(principal.ShipIDs) != 1 || principal.ShipIDs[0] != shipID {
		t.Fatalf("principal scope=%v", principal.ShipIDs)
	}
	// An observer whose only ship is removed from scope in the durable record
	// must not authenticate as a fleet-wide reader.
	snapshot := r.durableLocked()
	for i := range snapshot.Observers {
		snapshot.Observers[i].Ships = nil
	}
	if err := r.restoreLocked(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AuthenticateObserver(credential.CredentialID, credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("an observer with no ships authenticated: %v", err)
	}
}
