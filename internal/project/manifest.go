package project

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
)

type Manifest struct {
	Version string            `json:"version"`
	Files   map[string]string `json:"files"`
}

func LoadManifest() (*Manifest, error) {
	b, err := os.ReadFile(ManifestPath())
	if errors.Is(err, fs.ErrNotExist) {
		return &Manifest{Files: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	if m.Files == nil {
		m.Files = map[string]string{}
	}
	return &m, nil
}

func (m *Manifest) Save() error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ManifestPath(), b, 0o644)
}

func SHA(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
