package fleetidentity

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *fakeClock) Advance(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

type fakeRandom struct {
	mu   sync.Mutex
	next byte
}

func (r *fakeRandom) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range p {
		r.next++
		p[i] = r.next
	}
	return len(p), nil
}

func newTestRegistry(t *testing.T) (*Registry, *fakeClock) {
	t.Helper()
	c := &fakeClock{now: time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)}
	r, err := NewRegistry("flt_0123456789abcdef", c, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	return r, c
}
func enroll(t *testing.T, r *Registry) (EnrollmentArtifact, EnrollmentResult) {
	t.Helper()
	a, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	return a, res
}

func TestEnrollmentIsExplicitStableAndIdempotent(t *testing.T) {
	r, _ := newTestRegistry(t)
	a, res := enroll(t, r)
	again, err := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	if err != nil || again != res {
		t.Fatalf("lost response reconciliation: %#v %v", again, err)
	}
	if _, err = r.Enroll(a.ArtifactID, a.Secret, "txn_1123456789abcdef"); !IsCode(err, Unauthorized) {
		t.Fatalf("artifact replay: %v", err)
	}
	p, err := r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret)
	if err != nil || p.ShipID != res.ShipID {
		t.Fatalf("authenticate: %#v %v", p, err)
	}
}
func TestEnrollmentExpiryUsesFakeClock(t *testing.T) {
	r, c := newTestRegistry(t)
	a, err := r.CreateEnrollment(time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if _, err = r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef"); !IsCode(err, Expired) {
		t.Fatalf("exact expiry: %v", err)
	}
}
func TestRotationOverlapProofCommitAndExpiry(t *testing.T) {
	r, c := newTestRegistry(t)
	_, res := enroll(t, r)
	next, err := r.IssueShipRotation(res.ShipID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret); err != nil {
		t.Fatal(err)
	}
	if err = r.CommitShipRotation(res.ShipID, next.CredentialID); !IsCode(err, Conflict) {
		t.Fatalf("commit before proof: %v", err)
	}
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); err != nil {
		t.Fatal(err)
	}
	if err = r.CommitShipRotation(res.ShipID, next.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("old after commit: %v", err)
	}
	third, err := r.IssueShipRotation(res.ShipID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.Advance(time.Minute)
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("old after expiry: %v", err)
	}
	if _, err = r.AuthenticateShip(third.CredentialID, third.Secret); err != nil {
		t.Fatal(err)
	}
}
func TestSeparateObserverAuthorityAndRevocation(t *testing.T) {
	r, _ := newTestRegistry(t)
	_, res := enroll(t, r)
	o, err := r.IssueObserver([]string{res.ShipID})
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.AuthenticateObserver(o.CredentialID, o.Secret)
	if err != nil || p.Capability != ObserveCapability || len(p.ShipIDs) != 1 {
		t.Fatalf("observer: %#v %v", p, err)
	}
	if _, err = r.AuthenticateShip(o.CredentialID, o.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("observer used as ship: %v", err)
	}
	if _, err = r.AuthenticateObserver(res.Credential.CredentialID, res.Credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("ship used as observer: %v", err)
	}
	if err = r.RevokeObserver(o.CredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateObserver(o.CredentialID, o.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("revoked observer: %v", err)
	}
	if err = r.RevokeShip(res.ShipID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("revoked ship: %v", err)
	}
}
func TestConcurrentArtifactRedemptionAllocatesOneShip(t *testing.T) {
	r, _ := newTestRegistry(t)
	a, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan EnrollmentResult, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, e := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
			results <- v
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var first string
	for v := range results {
		if first == "" {
			first = v.ShipID
		} else if v.ShipID != first {
			t.Fatalf("two ships: %s %s", first, v.ShipID)
		}
	}
}

func validShipState() ShipState {
	return ShipState{SchemaVersion: 1, FleetID: "flt_0123456789abcdef", ShipID: "shp_0123456789abcdef", FleetDestination: "https://fleet.example", FleetServiceIdentity: "sha256:0123456789abcdef", CredentialID: "shc_0123456789abcdef", CredentialSecret: "shs_0123456789abcdefghijklmnopqrstuvwxyz"}
}
func TestShipStateRestrictiveAtomicAndExplicitReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "m7")
	plan, err := PlanShipState(dir)
	if err != nil || plan.Exists || plan.Path != filepath.Join(dir, shipStateFile) {
		t.Fatalf("read-only absent plan: %#v %v", plan, err)
	}
	s := validShipState()
	if err := StoreShipState(dir, s); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, shipStateFile))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	got, err := LoadShipState(dir)
	if err != nil || got != s {
		t.Fatalf("load: %#v %v", got, err)
	}
	if err = StoreShipState(dir, s); !IsCode(err, AlreadyExists) {
		t.Fatalf("implicit replace: %v", err)
	}
	s.CredentialID = "shc_1123456789abcdef"
	if err = ReplaceShipState(dir, s); err != nil {
		t.Fatal(err)
	}
	got, err = LoadShipState(dir)
	if err != nil || got.CredentialID != s.CredentialID {
		t.Fatalf("replace: %#v %v", got, err)
	}
}
func TestShipStateRefusesSymlinkAndWrongPermissions(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := StoreShipState(link, validShipState()); err == nil {
		t.Fatal("accepted symlink component")
	}
	dir := filepath.Join(root, "state")
	if err := StoreShipState(dir, validShipState()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, shipStateFile), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadShipState(dir); err == nil {
		t.Fatal("accepted permissive secret file")
	}
}
func TestDiagnosticsNeverContainSecrets(t *testing.T) {
	r, _ := newTestRegistry(t)
	a, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	secret := a.Secret + "canary"
	_, err = r.Enroll(a.ArtifactID, secret, "txn_0123456789abcdef")
	if err == nil || bytes.Contains([]byte(err.Error()), []byte(secret)) {
		t.Fatalf("unsafe error: %v", err)
	}
	var coded *Error
	if !errors.As(err, &coded) {
		t.Fatalf("not bounded typed diagnostic: %T", err)
	}
}

func TestPersistentAuthorityRestartRevocationReplayRotationAndGeneration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	c := &fakeClock{now: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	r, err := OpenRegistry(dir, "flt_0123456789abcdef", c, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	o, err := r.IssueObserver([]string{res.ShipID})
	if err != nil {
		t.Fatal(err)
	}
	p, err := r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret)
	if err != nil {
		t.Fatal(err)
	}
	g, err := r.AllocateConnectionGeneration(p)
	if err != nil || g != 1 {
		t.Fatalf("generation %d %v", g, err)
	}
	next, err := r.IssueShipRotation(res.ShipID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); err != nil {
		t.Fatal(err)
	}
	if err = r.CommitShipRotation(res.ShipID, next.CredentialID); err != nil {
		t.Fatal(err)
	}
	if err = r.RevokeObserver(o.CredentialID); err != nil {
		t.Fatal(err)
	}

	r, err = OpenRegistry(dir, "flt_0123456789abcdef", c, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	if err != nil || again != res {
		t.Fatalf("replay %#v %v", again, err)
	}
	if _, err = r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("old rotation credential: %v", err)
	}
	p, err = r.AuthenticateShip(next.CredentialID, next.Secret)
	if err != nil {
		t.Fatal(err)
	}
	g, err = r.AllocateConnectionGeneration(p)
	if err != nil || g != 2 {
		t.Fatalf("generation after restart %d %v", g, err)
	}
	if _, err = r.AuthenticateObserver(o.CredentialID, o.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("revoked observer restarted: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, authorityFile))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("authority permissions %v %v", info, err)
	}
}

func TestPersistentAuthorityFailedCommitRollsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	r, c := newTestRegistry(t)
	r.storeDir = dir
	if err := r.loadAuthority(); err != nil {
		t.Fatal(err)
	}
	a, err := r.CreateEnrollment(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	goodDir := r.storeDir
	r.storeDir = "relative-authority-path"
	if _, err = r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef"); err == nil {
		t.Fatal("commit failure accepted")
	}
	r.storeDir = goodDir
	if _, err = r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef"); err != nil {
		t.Fatalf("rollback lost artifact: %v", err)
	}
	_ = c
}

func TestRotationExpiryIsCommittedBeforeAuthenticationReturns(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	c := &fakeClock{now: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	r, err := OpenRegistry(dir, "flt_0123456789abcdef", c, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.CreateEnrollment(time.Hour)
	res, _ := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	next, err := r.IssueShipRotation(res.ShipID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c.now = c.now.Add(time.Minute)
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRegistry(dir, "flt_0123456789abcdef", c, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateShip(res.Credential.CredentialID, res.Credential.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("expired current survived restart: %v", err)
	}
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); err != nil {
		t.Fatalf("promoted credential missing: %v", err)
	}
}

func TestRotationExpiryCommitFailureFailsAuthenticationAndRollsBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	c := &fakeClock{now: time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)}
	r, err := OpenRegistry(dir, "flt_0123456789abcdef", c, &fakeRandom{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := r.CreateEnrollment(time.Hour)
	res, _ := r.Enroll(a.ArtifactID, a.Secret, "txn_0123456789abcdef")
	next, _ := r.IssueShipRotation(res.ShipID, time.Minute)
	c.now = c.now.Add(time.Minute)
	good := r.storeDir
	r.storeDir = "relative-authority-path"
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); err == nil {
		t.Fatal("authentication returned through failed expiry commit")
	}
	r.storeDir = good
	// The failed pre-rename commit restored the old authority; retry commits the
	// expiry and only then authenticates the promoted credential.
	if _, err = r.AuthenticateShip(next.CredentialID, next.Secret); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorCredentialRestartRotationRevocationScopeAndExpiry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	clock := &fakeClock{now: time.Unix(9000, 0)}
	r, err := OpenRegistry(dir, "fleet-000000000001", clock, &fakeRandom{next: 1})
	if err != nil {
		t.Fatal(err)
	}
	_, ship := enroll(t, r)
	issued, err := r.IssueOperator("operator-00000001", []string{ship.ShipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateObserver(issued.Record.CredentialID, issued.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("operator used as observer: %v", err)
	}
	if _, err = r.AuthenticateShip(issued.Record.CredentialID, issued.Secret); !IsCode(err, Unauthorized) {
		t.Fatalf("operator used as ship: %v", err)
	}
	r, err = OpenRegistry(dir, "fleet-000000000001", clock, &fakeRandom{next: 50})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateOperator(issued.Record.CredentialID, issued.Secret, "fleet-000000000001", ship.ShipID); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateOperator(issued.Record.CredentialID, issued.Secret, "fleet-wrong-000001", ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("wrong fleet: %v", err)
	}
	rotated, err := r.RotateOperator(issued.Record.CredentialID, 1, 5*time.Minute, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Record.CredentialGeneration != 2 || rotated.Record.SubjectID != issued.Record.SubjectID || !reflect.DeepEqual(rotated.Record.ShipIDs, issued.Record.ShipIDs) {
		t.Fatalf("rotation=%+v", rotated.Record)
	}
	if err = r.CommitOperatorRotation(rotated.Record.CredentialID, 2); err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateOperator(issued.Record.CredentialID, issued.Secret, issued.Record.FleetID, ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("old generation survived: %v", err)
	}
	if err = r.RevokeOperatorSubject(rotated.Record.SubjectID); err != nil {
		t.Fatal(err)
	}
	r, err = OpenRegistry(dir, "fleet-000000000001", clock, &fakeRandom{next: 90})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.AuthenticateOperator(rotated.Record.CredentialID, rotated.Secret, rotated.Record.FleetID, ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("revocation survived restart: %v", err)
	}
	short, err := r.IssueOperator("operator-00000002", []string{ship.ShipID}, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	clock.Advance(time.Minute)
	if _, err = r.AuthenticateOperator(short.Record.CredentialID, short.Secret, short.Record.FleetID, ship.ShipID); !IsCode(err, Unauthorized) {
		t.Fatalf("expiry: %v", err)
	}
}

func TestInterruptCredentialIsSeparateDurableAndCapabilityDark(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "authority")
	clock := &fakeClock{now: time.Unix(1700000000, 0)}
	r, err := OpenRegistry(dir, "fleet-000000000001", clock, &fakeRandom{next: 90})
	if err != nil {
		t.Fatal(err)
	}
	_, ship := enroll(t, r)
	steer, err := r.IssueOperator("operator-00000001", []string{ship.ShipID}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	intr, err := r.IssueOperatorCapability("operator-00000001", []string{ship.ShipID}, InterruptTurnCapability, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if steer.Record.Capability != SteerTurnCapability || intr.Record.Capability != InterruptTurnCapability || steer.Record.CredentialID == intr.Record.CredentialID {
		t.Fatal("capabilities were not separately issued")
	}
	if p, err := r.AuthenticateOperator(intr.Record.CredentialID, intr.Secret, intr.Record.FleetID, ship.ShipID); err != nil || p.Capability != InterruptTurnCapability {
		t.Fatalf("interrupt auth = %#v, %v", p, err)
	}
	r, err = OpenRegistry(dir, "fleet-000000000001", clock, &fakeRandom{next: 120})
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := r.RotateOperator(intr.Record.CredentialID, 1, time.Minute, time.Hour)
	if err != nil || rotated.Record.Capability != InterruptTurnCapability {
		t.Fatalf("rotation = %#v, %v", rotated.Record, err)
	}
}

func TestOperatorIssuanceCommitFailureRollsBack(t *testing.T) {
	r, _ := newTestRegistry(t)
	_, ship := enroll(t, r)
	r.storeDir = filepath.Join(t.TempDir(), "parent-file", "authority")
	if err := os.WriteFile(filepath.Dir(r.storeDir), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := r.IssueOperator("operator-00000001", []string{ship.ShipID}, time.Hour); err == nil {
		t.Fatal("expected storage failure")
	}
	if len(r.operators) != 0 {
		t.Fatal("failed issuance remained active")
	}
}
