package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// ManifestCommitUncertainError reports that publication crossed the rename
// boundary but directory durability could not be confirmed.
type ManifestCommitUncertainError struct{ Err error }

func (e *ManifestCommitUncertainError) Error() string {
	return "manifest commit durability uncertain: " + e.Err.Error()
}
func (e *ManifestCommitUncertainError) Unwrap() error { return e.Err }

func SerializeManifestV2(manifest *Manifest) ([]byte, error) {
	if manifest == nil || manifest.Version != ManifestVersion || manifest.Files == nil {
		return nil, errors.New("invalid manifest v2")
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// CommitManifestV2 atomically publishes a complete canonical manifest.
func CommitManifestV2(root string, manifest *Manifest) error {
	raw, err := SerializeManifestV2(manifest)
	if err != nil {
		return err
	}
	dir := filepath.Join(root, Dir)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".manifest-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, filepath.Join(dir, ManifestName)); err != nil {
		return err
	}
	ok = true
	return nil
}
