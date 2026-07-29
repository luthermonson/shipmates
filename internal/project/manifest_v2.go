package project

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ManifestCommitUncertainError reports that publication crossed the rename
// boundary but directory durability could not be confirmed.
type ManifestCommitUncertainError struct{ Err error }

func (e *ManifestCommitUncertainError) Error() string {
	return "manifest commit durability uncertain: " + e.Err.Error()
}
func (e *ManifestCommitUncertainError) Unwrap() error { return e.Err }

// legacyManifestVersions are the version values written by releases that
// predate the versioned manifest. Before v2 existed, Manifest.Save marshalled
// the struct with no default, so every project on disk carries `"version": ""`.
// "1" is accepted defensively for any manifest that was stamped by hand.
var legacyManifestVersions = map[string]bool{"": true, "1": true}

// MigrateManifest upgrades a pre-v2 manifest to v2 in place and republishes it
// atomically, reporting whether anything changed. The recorded file hashes are
// carried across verbatim: they are the only record of which artifacts shipmates
// owns, and the legacy `.claude` artifact paths they name are still live. A
// version this build does not recognise is refused with a recoverable error
// rather than being rewritten.
func MigrateManifest(root string, manifest *Manifest) (bool, error) {
	path := filepath.Join(root, ManifestPath())
	if manifest == nil {
		return false, errors.New("no install manifest was loaded")
	}
	if manifest.Version == ManifestVersion {
		if manifest.Files == nil {
			return false, fmt.Errorf("%s is missing its files map; restore it from version control", path)
		}
		return false, nil
	}
	if !legacyManifestVersions[manifest.Version] {
		return false, fmt.Errorf("%s reports manifest version %q, which this build of shipmates does not understand; upgrade shipmates or restore a manifest written by this version", path, manifest.Version)
	}
	if manifest.Files == nil {
		manifest.Files = map[string]string{}
	}
	if err := validateLegacyManifestFiles(path, manifest.Files); err != nil {
		return false, err
	}
	manifest.Version = ManifestVersion
	// A project that has no manifest on disk yet (a fresh `init`) has nothing to
	// republish; normalising the in-memory value is the whole migration.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	if err := CommitManifestV2(root, manifest); err != nil {
		return false, fmt.Errorf("upgrade %s to manifest v2: %w", path, err)
	}
	return true, nil
}

// validateLegacyManifestFiles refuses to stamp v2 onto a files map that is not
// a manifest, so a corrupt or hand-mangled file produces an actionable error
// instead of silently becoming an authoritative v2 record. Keys are compared
// permissively: manifests written on Windows contain backslash separators.
func validateLegacyManifestFiles(path string, files map[string]string) error {
	for rel, sha := range files {
		if rel == "" || filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
			return fmt.Errorf("%s records an unusable path %q; fix or remove that entry", path, rel)
		}
		for _, part := range strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == ".." {
				return fmt.Errorf("%s records an unusable path %q; fix or remove that entry", path, rel)
			}
		}
		if len(sha) != 64 {
			return fmt.Errorf("%s records a malformed hash for %q; fix or remove that entry", path, rel)
		}
		for _, c := range sha {
			if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
				return fmt.Errorf("%s records a malformed hash for %q; fix or remove that entry", path, rel)
			}
		}
	}
	return nil
}

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
