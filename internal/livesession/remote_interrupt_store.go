package livesession

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

const remoteInterruptStoreFile = "remote-interrupt-operations.json"

type durableInterruptRecord struct {
	Fingerprint             string                  `json:"fingerprint"`
	Request                 RemoteInterruptRequest  `json:"request"`
	Target                  RemoteInterruptTarget   `json:"target"`
	Operator                RemoteInterruptOperator `json:"operator"`
	FirstReceipt, ExpiresAt time.Time
	Committing              bool                          `json:"committing"`
	Result                  RemoteInterruptResult         `json:"result"`
	Audit                   RemoteInterruptAuditCandidate `json:"audit"`
}
type durableInterruptTomb struct {
	OperationID, Nonce string
	ExpiresAt          time.Time
}
type durableInterruptStore struct {
	Schema  int                      `json:"schema"`
	Records []durableInterruptRecord `json:"records"`
	Tombs   []durableInterruptTomb   `json:"tombstones"`
}

func (c *RemoteInterruptCoordinator) durableLocked() durableInterruptStore {
	d := durableInterruptStore{Schema: 1}
	for _, t := range c.tombs {
		d.Tombs = append(d.Tombs, durableInterruptTomb{t.operationID, t.nonce, t.expiresAt})
	}
	for _, r := range c.records {
		d.Records = append(d.Records, durableInterruptRecord{base64.RawURLEncoding.EncodeToString(r.fingerprint[:]), r.request, r.target, r.operator, r.firstReceipt, r.expiresAt, r.committing, r.result, r.audit})
	}
	return d
}
func (c *RemoteInterruptCoordinator) loadRemoteInterruptStore() error {
	if err := secureStateDir(c.storeDir, true); err != nil {
		return err
	}
	p := filepath.Join(c.storeDir, remoteInterruptStoreFile)
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return c.persistLocked()
	}
	if err != nil || len(b) > 4<<20 {
		return errors.New("remote interrupt storage unavailable")
	}
	var d durableInterruptStore
	if json.Unmarshal(b, &d) != nil || d.Schema != 1 || len(d.Records) > RemoteInterruptMaxRecords || len(d.Tombs) > remoteInterruptMaxTombs {
		return errors.New("remote interrupt storage unavailable")
	}
	st, err := os.Lstat(p)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return errors.New("remote interrupt storage unavailable")
	}
	for _, x := range d.Records {
		fb, e := base64.RawURLEncoding.DecodeString(x.Fingerprint)
		if e != nil || len(fb) != 32 || x.Request.OperationID == "" || x.Request.OperationNonce == "" || c.records[x.Request.OperationID] != nil || c.nonces[x.Request.OperationNonce] != "" {
			return errors.New("remote interrupt storage unavailable")
		}
		var fp [32]byte
		copy(fp[:], fb)
		r := &remoteInterruptRecord{fingerprint: fp, request: x.Request, target: x.Target, operator: x.Operator, firstReceipt: x.FirstReceipt, expiresAt: x.ExpiresAt, committing: x.Committing, result: x.Result, audit: x.Audit, done: make(chan struct{})}
		if !r.committing {
			close(r.done)
		}
		c.records[x.Request.OperationID], c.nonces[x.Request.OperationNonce] = r, x.Request.OperationID
	}
	for _, t := range d.Tombs {
		if t.OperationID == "" || t.Nonce == "" {
			return errors.New("remote interrupt storage unavailable")
		}
		c.tombs = append(c.tombs, remoteInterruptTomb{t.OperationID, t.Nonce, t.ExpiresAt})
	}
	return nil
}
func (c *RemoteInterruptCoordinator) persistLocked() error {
	if c.storeDir == "" {
		return nil
	}
	if err := secureStateDir(c.storeDir, true); err != nil {
		return err
	}
	b, err := json.Marshal(c.durableLocked())
	if err != nil {
		return errors.New("remote interrupt storage unavailable")
	}
	b = append(b, '\n')
	tmp, dst := filepath.Join(c.storeDir, ".remote-interrupt.tmp"), filepath.Join(c.storeDir, remoteInterruptStoreFile)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("remote interrupt storage unavailable")
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(b); err != nil || f.Sync() != nil || f.Close() != nil || os.Rename(tmp, dst) != nil {
		return errors.New("remote interrupt storage unavailable")
	}
	d, err := os.Open(c.storeDir)
	if err != nil {
		return errors.New("remote interrupt storage unavailable")
	}
	err = d.Sync()
	_ = d.Close()
	if err != nil {
		return errors.New("remote interrupt storage unavailable")
	}
	ok = true
	return nil
}
