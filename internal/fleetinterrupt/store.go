package fleetinterrupt

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/luthermonson/shipmates/internal/fleetidentity"
	"github.com/luthermonson/shipmates/internal/livesession"
)

const fleetStoreFile = "fleet-interrupt-operations.json"

type appendAudit struct {
	file    *os.File
	failure func() error
}
type auditLine struct {
	SchemaVersion                                                                                   int       `json:"schema_version"`
	Kind                                                                                            string    `json:"kind"`
	At                                                                                              time.Time `json:"at"`
	OperationID, SubjectID, CredentialID, Capability, FleetID, ShipID, Persona, TargetHash, Outcome string
	CredentialGeneration, FleetEpoch, ConnectionGeneration                                          uint64
}

func openAudit(path string) (*appendAudit, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, errors.New("interrupt audit unavailable")
	}
	if st, err := os.Lstat(filepath.Dir(path)); err != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("interrupt audit unavailable")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return nil, errors.New("interrupt audit unavailable")
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		f.Close()
		return nil, errors.New("interrupt audit unavailable")
	}
	return &appendAudit{file: f}, nil
}
func (a *appendAudit) appendFleet(at time.Time, kind string, p fleetidentity.OperatorPrincipal, in SubmitV1, op, outcome string) error {
	if a.failure != nil {
		if err := a.failure(); err != nil {
			return err
		}
	}
	h := sha256.Sum256([]byte(in.InterruptTargetRef))
	x := auditLine{1, kind, at, op, p.SubjectID, p.CredentialID, p.Capability, in.FleetID, in.ShipID, in.Persona, base64.RawURLEncoding.EncodeToString(h[:]), outcome, p.CredentialGeneration, in.FleetEpoch, in.ConnectionGeneration}
	b, err := json.Marshal(x)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if _, err = a.file.Write(b); err != nil {
		return err
	}
	return a.file.Sync()
}

type durableFleetOperation struct {
	Fingerprint string
	Principal   fleetidentity.OperatorPrincipal
	Input       SubmitV1
	Nonce       string
	Result      livesession.RemoteInterruptResult
	Finished    bool
	Created     time.Time
}
type durableFleetStore struct {
	Schema     int                     `json:"schema"`
	Operations []durableFleetOperation `json:"operations"`
}

// OpenService enables the production-required durable operation and append-only
// attempted-audit sinks. Any unfinished operation is classified as uncertain
// after Fleet restart and remains queryable/replayable by its exact ID.
func OpenService(fleetID string, authority Authority, random io.Reader, dir string) (*Service, error) {
	return OpenServiceWithConfig(fleetID, authority, random, dir, ServiceConfig{})
}
func OpenServiceWithConfig(fleetID string, authority Authority, random io.Reader, dir string, config ServiceConfig) (*Service, error) {
	s, err := NewServiceWithConfig(fleetID, authority, random, config)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, errors.New("invalid_request")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	if st, e := os.Lstat(dir); e != nil || !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, errors.New("remote interrupt storage unavailable")
	}
	s.storeDir = dir
	s.audit, err = openAudit(filepath.Join(dir, "fleet-interrupt-attempted.audit"))
	if err != nil {
		return nil, err
	}
	if err = s.loadLocked(); err != nil {
		return nil, err
	}
	return s, nil
}
func (s *Service) persistLocked() error {
	if s.persistFailure != nil {
		if err := s.persistFailure(); err != nil {
			return err
		}
	}
	if s.storeDir == "" {
		return nil
	}
	d := durableFleetStore{Schema: 1}
	for _, r := range s.operations {
		d.Operations = append(d.Operations, durableFleetOperation{base64.RawURLEncoding.EncodeToString(r.fingerprint[:]), r.principal, r.input, r.nonce, r.result, r.finished, r.created})
	}
	b, e := json.Marshal(d)
	if e != nil {
		return e
	}
	b = append(b, '\n')
	tmp := filepath.Join(s.storeDir, ".fleet-interrupt.tmp")
	dst := filepath.Join(s.storeDir, fleetStoreFile)
	f, e := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, e = f.Write(b); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if e = os.Rename(tmp, dst); e != nil {
		return e
	}
	dfile, e := os.Open(s.storeDir)
	if e != nil {
		return e
	}
	e = dfile.Sync()
	dfile.Close()
	ok = e == nil
	return e
}
func (s *Service) loadLocked() error {
	p := filepath.Join(s.storeDir, fleetStoreFile)
	f, e := os.Open(p)
	if errors.Is(e, os.ErrNotExist) {
		return s.persistLocked()
	}
	if e != nil {
		return e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 4<<20 {
		return errors.New("remote interrupt storage unavailable")
	}
	var d durableFleetStore
	dec := json.NewDecoder(bufio.NewReader(f))
	if dec.Decode(&d) != nil || d.Schema != 1 || len(d.Operations) > MaxFleetOperations {
		return errors.New("remote interrupt storage unavailable")
	}
	for _, x := range d.Operations {
		fb, e := base64.RawURLEncoding.DecodeString(x.Fingerprint)
		if e != nil || len(fb) != 32 || !validOperationID(x.Result.OperationID) || s.operations[x.Result.OperationID] != nil {
			return errors.New("remote interrupt storage unavailable")
		}
		var fp [32]byte
		copy(fp[:], fb)
		if !x.Finished {
			x.Result = result(x.Result.OperationID, livesession.RemoteInterruptIndeterminate, "fleet_restarted")
			x.Finished = true
		}
		ch := make(chan struct{})
		close(ch)
		s.operations[x.Result.OperationID] = &fleetOperation{fp, x.Principal, x.Input, x.Nonce, x.Result, ch, true, x.Created}
	}
	return s.persistLocked()
}
