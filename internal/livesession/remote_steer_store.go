package livesession

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const remoteSteerStoreFile = "remote-steer-operations.json"

type durableRemoteRecord struct {
	Fingerprint             string              `json:"fingerprint"`
	Request                 RemoteSteerRequest  `json:"request"`
	Target                  RemoteSteerTarget   `json:"target"`
	Operator                RemoteSteerOperator `json:"operator"`
	FirstReceipt, ExpiresAt time.Time
	Committing              bool                      `json:"committing"`
	Result                  RemoteSteerResult         `json:"result"`
	Audit                   RemoteSteerAuditCandidate `json:"audit"`
	TerminalObserved        bool                      `json:"terminal_observed"`
}
type durableRemoteStore struct {
	Schema  int                   `json:"schema"`
	Records []durableRemoteRecord `json:"records"`
	Tombs   []durableRemoteTomb   `json:"tombstones"`
}
type durableRemoteTomb struct {
	OperationID, Nonce string
	ExpiresAt          time.Time
}

func (c *RemoteSteerCoordinator) durableLocked() durableRemoteStore {
	d := durableRemoteStore{Schema: 1}
	for _, t := range c.tombs {
		d.Tombs = append(d.Tombs, durableRemoteTomb{t.operationID, t.nonce, t.expiresAt})
	}
	for _, r := range c.records {
		d.Records = append(d.Records, durableRemoteRecord{base64.RawURLEncoding.EncodeToString(r.fingerprint[:]), r.request, r.target, r.operator, r.firstReceipt, r.expiresAt, r.committing, r.result, r.audit, r.terminalObserved})
	}
	return d
}
func (c *RemoteSteerCoordinator) loadRemoteSteerStore() error {
	if err := secureStateDir(c.storeDir, true); err != nil {
		return err
	}
	p := filepath.Join(c.storeDir, remoteSteerStoreFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return c.persistLocked()
	}
	if err != nil || len(b) > 4<<20 {
		return errors.New("remote steer storage unavailable")
	}
	var d durableRemoteStore
	if json.Unmarshal(b, &d) != nil || d.Schema != 1 || len(d.Records) > RemoteSteerMaxRecords || len(d.Tombs) > remoteSteerMaxTombs {
		return errors.New("remote steer storage unavailable")
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return errors.New("remote steer storage unavailable")
	}
	for _, x := range d.Records {
		fb, e := base64.RawURLEncoding.DecodeString(x.Fingerprint)
		if e != nil || len(fb) != 32 || x.Request.OperationID == "" || x.Request.OperationNonce == "" || c.records[x.Request.OperationID] != nil || c.nonces[x.Request.OperationNonce] != "" {
			return errors.New("remote steer storage unavailable")
		}
		var fp [32]byte
		copy(fp[:], fb)
		r := &remoteSteerRecord{fingerprint: fp, request: x.Request, target: x.Target, operator: x.Operator, firstReceipt: x.FirstReceipt, expiresAt: x.ExpiresAt, committing: x.Committing, result: x.Result, audit: x.Audit, done: make(chan struct{}), terminalObserved: x.TerminalObserved}
		if !r.committing {
			close(r.done)
		}
		c.records[x.Request.OperationID], c.nonces[x.Request.OperationNonce] = r, x.Request.OperationID
	}
	for _, t := range d.Tombs {
		if t.OperationID == "" || t.Nonce == "" {
			return errors.New("remote steer storage unavailable")
		}
		c.tombs = append(c.tombs, remoteSteerTomb{t.OperationID, t.Nonce, t.ExpiresAt})
	}
	return nil
}
func (c *RemoteSteerCoordinator) persistLocked() error {
	if c.storeDir == "" {
		return nil
	}
	if err := secureStateDir(c.storeDir, true); err != nil {
		return err
	}
	b, err := json.Marshal(c.durableLocked())
	if err != nil {
		return errors.New("remote steer storage unavailable")
	}
	b = append(b, '\n')
	tmp := filepath.Join(c.storeDir, ".remote-steer.tmp")
	dst := filepath.Join(c.storeDir, remoteSteerStoreFile)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("remote steer storage unavailable")
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(b); err != nil || f.Sync() != nil || f.Close() != nil || os.Rename(tmp, dst) != nil {
		return errors.New("remote steer storage unavailable")
	}
	d, err := os.Open(c.storeDir)
	if err != nil {
		return errors.New("remote steer storage unavailable")
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return errors.New("remote steer storage unavailable")
	}
	ok = true
	return nil
}
func secureStateDir(path string, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return errors.New("remote steer storage unavailable")
	}
	if create {
		parts := []string{}
		for p := path; p != string(filepath.Separator); p = filepath.Dir(p) {
			parts = append(parts, filepath.Base(p))
		}
		p := string(filepath.Separator)
		for i := len(parts) - 1; i >= 0; i-- {
			p = filepath.Join(p, parts[i])
			st, err := os.Lstat(p)
			if errors.Is(err, os.ErrNotExist) {
				if os.Mkdir(p, 0o700) != nil {
					return errors.New("remote steer storage unavailable")
				}
				st, err = os.Lstat(p)
			}
			if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
				return errors.New("remote steer storage unavailable")
			}
		}
	}
	for p := path; ; p = filepath.Dir(p) {
		st, err := os.Lstat(p)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return errors.New("remote steer storage unavailable")
		}
		if p == path && st.Mode().Perm()&0o077 != 0 {
			return errors.New("remote steer storage unavailable")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	return nil
}
