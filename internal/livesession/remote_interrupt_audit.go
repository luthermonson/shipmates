package livesession

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

const remoteInterruptAuditFile = "remote-interrupt.audit"

type remoteInterruptFileAudit struct {
	mu sync.Mutex
	f  *os.File
}

func openRemoteInterruptAuditSink(dir string) (*remoteInterruptFileAudit, error) {
	if err := secureStateDir(dir, true); err != nil {
		return nil, errors.New("remote interrupt audit unavailable")
	}
	p := filepath.Join(dir, remoteInterruptAuditFile)
	f, err := openPrivateAppendFile(p)
	if err != nil {
		return nil, errors.New("remote interrupt audit unavailable")
	}
	return &remoteInterruptFileAudit{f: f}, nil
}

func (a *remoteInterruptFileAudit) AppendRemoteInterrupt(v RemoteInterruptAuditCandidate) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return errors.New("remote interrupt audit unavailable")
	}
	b = append(b, '\n')
	if _, err = a.f.Write(b); err != nil || a.f.Sync() != nil {
		return errors.New("remote interrupt audit unavailable")
	}
	return nil
}

func readRemoteInterruptAudit(path string) ([]RemoteInterruptAuditCandidate, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []RemoteInterruptAuditCandidate
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var v RemoteInterruptAuditCandidate
		if json.Unmarshal(s.Bytes(), &v) != nil {
			return nil, errors.New("remote interrupt audit unavailable")
		}
		out = append(out, v)
	}
	return out, s.Err()
}
