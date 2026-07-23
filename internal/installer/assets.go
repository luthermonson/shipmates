// Package installer owns the offline, fixed-layout Shipmates runtime installer.
// It deliberately has no network, credential, or service-manager dependency.
package installer

import (
	"bytes"
	"crypto/sha256"
	"debug/elf"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"sort"
	"strings"
)

//go:embed assets/*
var releaseAssets embed.FS

const (
	ReleaseVersion         = "shipmates-runtime-v1"
	ReleaseSequence uint64 = 1
	InstallerFormat        = "shipmates.installer.v1"
	installRoot            = "/usr/libexec/shipmates"
	stateRoot              = "/var/lib/shipmates"
	journalPath            = "/var/lib/shipmates/install.json"
	uninstallPath          = "/var/lib/shipmates/uninstall.json"
)

type AssetType string

const (
	Regular AssetType = "regular"
)

// Asset is the closed generated release manifest entry. Destination is never
// taken from an input file or command-line argument.
type Asset struct {
	Role        string    `json:"role"`
	Source      string    `json:"source"`
	Destination string    `json:"destination"`
	Digest      string    `json:"sha256"`
	Size        int64     `json:"size"`
	Type        AssetType `json:"type"`
	Mode        uint32    `json:"mode"`
	Owner       string    `json:"owner"`
	BuildID     string    `json:"build_id,omitempty"`
}

type Manifest struct {
	Schema           string  `json:"schema"`
	Format           string  `json:"format"`
	Release          string  `json:"release"`
	Sequence         uint64  `json:"sequence"`
	DescriptorSchema string  `json:"descriptor_schema"`
	MinInstaller     uint64  `json:"min_installer"`
	MaxInstaller     uint64  `json:"max_installer"`
	Assets           []Asset `json:"assets"`
}

// ReleaseDescriptor is stored inside each immutable release. It is separate
// from the manifest so a pointer can be checked before any candidate is
// activated, including when an older installer is inspecting a newer release.
type ReleaseDescriptor struct {
	Schema         string   `json:"schema"`
	Format         string   `json:"format"`
	Release        string   `json:"release"`
	Sequence       uint64   `json:"sequence"`
	Manifest       string   `json:"manifest_sha256"`
	ManifestSchema string   `json:"manifest_schema"`
	Roles          []string `json:"roles"`
	MinInstaller   uint64   `json:"min_installer"`
	MaxInstaller   uint64   `json:"max_installer"`
}

// ReleaseIdentity is the checked predecessor/candidate tuple used by an
// upgrade transaction. Pointer and inventory are supplied by the journal,
// never discovered from arbitrary filesystem paths.
type ReleaseIdentity struct {
	Descriptor ReleaseDescriptor
	Manifest   Manifest
	Pointer    string
	Inventory  []Inventory
}

const (
	releaseManifestName   = ".shipmates-manifest.json"
	releaseDescriptorName = ".shipmates-release.json"
)

func ManifestFor() (Manifest, error) {
	return manifestFor(true)
}

func manifestFor(includeUnit bool) (Manifest, error) {
	entries := []struct {
		role, source, destination string
		mode                      uint32
	}{
		{"qualifier-runner", "payloads/" + payloadArch + "/shipmates-m3-qualifier-run", "/usr/libexec/shipmates/shipmates-m3-qualifier-run", 0755},
		{"cgroup-launcher", "payloads/" + payloadArch + "/shipmates-cgroup-launcher", "/usr/libexec/shipmates/shipmates-cgroup-launcher", 0755},
		{"qualifier-unit", "assets/shipmates-m3-qualifier.service", "/etc/systemd/system/shipmates-m3-qualifier.service", 0644},
	}
	if !includeUnit {
		entries = entries[:2]
	}
	assets := make([]Asset, 0, len(entries))
	for _, e := range entries {
		if !fixedDestination(e.destination) {
			return Manifest{}, errors.New("manifest_destination_invalid")
		}
		b, err := assetBytes(e.source)
		if err != nil {
			return Manifest{}, errors.New("release_asset_unavailable")
		}
		sum := sha256.Sum256(b)
		entry := Asset{Role: e.role, Source: e.source, Destination: e.destination, Digest: hex.EncodeToString(sum[:]), Size: int64(len(b)), Type: Regular, Mode: e.mode, Owner: "root:root"}
		if strings.HasPrefix(e.source, "payloads/") {
			entry.BuildID, err = executableBuildID(b)
			if err != nil {
				return Manifest{}, errors.New("executable_payload_invalid")
			}
		}
		assets = append(assets, entry)
	}
	sort.Slice(assets, func(i, j int) bool { return assets[i].Destination < assets[j].Destination })
	m := Manifest{Schema: "shipmates.install.manifest.v1", Format: InstallerFormat, Release: ReleaseVersion, Sequence: ReleaseSequence, DescriptorSchema: "shipmates.install.descriptor.v1", MinInstaller: 1, MaxInstaller: 1, Assets: assets}
	return m, nil
}

func fixedDestination(destination string) bool {
	return destination == "/usr/libexec/shipmates/shipmates-m3-qualifier-run" || destination == "/usr/libexec/shipmates/shipmates-cgroup-launcher" || destination == "/etc/systemd/system/shipmates-m3-qualifier.service"
}

func validateManifest(m Manifest) error {
	if m.Schema != "shipmates.install.manifest.v1" || m.Format != InstallerFormat || m.Release != ReleaseVersion || m.Sequence != ReleaseSequence || m.DescriptorSchema != "shipmates.install.descriptor.v1" || m.MinInstaller != 1 || m.MaxInstaller != 1 || (len(m.Assets) != 2 && len(m.Assets) != 3) {
		return errors.New("manifest_invalid")
	}
	// The manifest is a closed release description, not a general-purpose
	// installer input.  Keep the role, source, destination, type, owner, mode,
	// digest, and size coupled to the embedded bytes so a structurally valid
	// substituted entry cannot acquire an install destination.
	expected := make(map[string]Asset, 3)
	for _, e := range []struct {
		role, source, destination string
		mode                      uint32
	}{
		{"qualifier-runner", "payloads/" + payloadArch + "/shipmates-m3-qualifier-run", "/usr/libexec/shipmates/shipmates-m3-qualifier-run", 0755},
		{"cgroup-launcher", "payloads/" + payloadArch + "/shipmates-cgroup-launcher", "/usr/libexec/shipmates/shipmates-cgroup-launcher", 0755},
		{"qualifier-unit", "assets/shipmates-m3-qualifier.service", "/etc/systemd/system/shipmates-m3-qualifier.service", 0644},
	} {
		b, err := assetBytes(e.source)
		if err != nil {
			return errors.New("release_asset_unavailable")
		}
		sum := sha256.Sum256(b)
		entry := Asset{Role: e.role, Source: e.source, Destination: e.destination, Digest: hex.EncodeToString(sum[:]), Size: int64(len(b)), Type: Regular, Mode: e.mode, Owner: "root:root"}
		if strings.HasPrefix(e.source, "payloads/") {
			entry.BuildID, err = executableBuildID(b)
			if err != nil {
				return errors.New("executable_payload_invalid")
			}
		}
		expected[e.destination] = entry
	}
	seen := make(map[string]bool, len(m.Assets))
	for _, a := range m.Assets {
		e, ok := expected[a.Destination]
		if !ok || seen[a.Destination] || a != e {
			return errors.New("manifest_invalid")
		}
		seen[a.Destination] = true
	}
	if !seen["/usr/libexec/shipmates/shipmates-m3-qualifier-run"] || !seen["/usr/libexec/shipmates/shipmates-cgroup-launcher"] {
		return errors.New("manifest_invalid")
	}
	return nil
}

func descriptorFor(m Manifest) ReleaseDescriptor {
	roles := make([]string, 0, len(m.Assets))
	for _, a := range m.Assets {
		roles = append(roles, a.Role)
	}
	sort.Strings(roles)
	return ReleaseDescriptor{Schema: "shipmates.install.descriptor.v1", Format: m.Format, Release: m.Release, Sequence: m.Sequence, Manifest: manifestHash(m), ManifestSchema: m.Schema, Roles: roles, MinInstaller: m.MinInstaller, MaxInstaller: m.MaxInstaller}
}

func validateDescriptor(d ReleaseDescriptor, m Manifest) error {
	if d.Schema != "shipmates.install.descriptor.v1" || d.Format != InstallerFormat || d.Release != m.Release || d.Sequence == 0 || d.ManifestSchema != m.Schema || d.Manifest != manifestHash(m) || d.MinInstaller > 1 || d.MaxInstaller < 1 || d.MinInstaller > d.MaxInstaller {
		return errors.New("release_descriptor_incompatible")
	}
	want := descriptorFor(m)
	if d.Sequence != want.Sequence || d.Release != want.Release || d.Format != want.Format || d.Manifest != want.Manifest || len(d.Roles) != len(want.Roles) {
		return errors.New("release_descriptor_incompatible")
	}
	for i := range want.Roles {
		if d.Roles[i] != want.Roles[i] {
			return errors.New("release_descriptor_incompatible")
		}
	}
	return nil
}

// ValidateUpgrade applies the cross-release compatibility contract before an
// activation intent is written. It is public for offline package/release
// verification and has no filesystem or service-manager side effects.
func ValidateUpgrade(predecessor, candidate ReleaseIdentity) error {
	if validateDescriptor(predecessor.Descriptor, predecessor.Manifest) != nil || validateDescriptor(candidate.Descriptor, candidate.Manifest) != nil {
		return errors.New("release_compatibility_invalid")
	}
	if predecessor.Pointer != predecessor.Descriptor.Release || candidate.Pointer != candidate.Descriptor.Release || len(predecessor.Inventory) != len(predecessor.Manifest.Assets) || len(candidate.Inventory) != len(candidate.Manifest.Assets) {
		return errors.New("release_pointer_invalid")
	}
	if candidate.Descriptor.Sequence <= predecessor.Descriptor.Sequence {
		if candidate.Descriptor.Sequence == predecessor.Descriptor.Sequence && candidate.Descriptor.Manifest != predecessor.Descriptor.Manifest {
			return errors.New("release_equal_changed")
		}
		return errors.New("release_downgrade")
	}
	if candidate.Descriptor.Format != predecessor.Descriptor.Format || candidate.Descriptor.ManifestSchema != predecessor.Descriptor.ManifestSchema {
		return errors.New("release_compatibility_invalid")
	}
	return nil
}

func inventoryForManifest(m Manifest) []Inventory {
	inv := make([]Inventory, 0, len(m.Assets))
	for _, a := range m.Assets {
		inv = append(inv, Inventory{Destination: a.Destination, Digest: a.Digest, Size: a.Size, Mode: a.Mode, Owner: a.Owner})
	}
	return inv
}

// ReleaseDescriptorFor exposes the canonical descriptor used by release
// tooling without permitting callers to choose roles or destinations.
func ReleaseDescriptorFor() (ReleaseDescriptor, error) {
	m, err := ManifestFor()
	if err != nil {
		return ReleaseDescriptor{}, err
	}
	return descriptorFor(m), nil
}

func descriptorHash(d ReleaseDescriptor) string { b, _ := json.Marshal(d); return sha256Hex(b) }

func assetBytes(source string) ([]byte, error) {
	if strings.HasPrefix(source, "payloads/") {
		return targetPayloads.ReadFile(source)
	}
	return fs.ReadFile(releaseAssets, source)
}

// PayloadFor returns a copy of the exact target-matched bytes embedded in the
// installer. Release tooling uses this to attest extraction equality.
func PayloadFor(role string) ([]byte, error) {
	var source string
	switch role {
	case "qualifier-runner":
		source = "payloads/" + payloadArch + "/shipmates-m3-qualifier-run"
	case "cgroup-launcher":
		source = "payloads/" + payloadArch + "/shipmates-cgroup-launcher"
	default:
		return nil, errors.New("payload_role_unknown")
	}
	b, err := assetBytes(source)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), b...), nil
}

func executableBuildID(b []byte) (string, error) {
	f, err := elf.NewFile(bytes.NewReader(b))
	if err != nil {
		return "", err
	}
	defer f.Close()
	if f.Class != elf.ELFCLASS64 || f.Data != elf.ELFDATA2LSB {
		return "", errors.New("elf_target_mismatch")
	}
	want := elf.EM_X86_64
	if payloadArch == "linux_arm64" {
		want = elf.EM_AARCH64
	}
	if f.Machine != want {
		return "", errors.New("elf_target_mismatch")
	}
	for _, p := range f.Progs {
		if p.Type == elf.PT_INTERP {
			return "", errors.New("elf_dynamic_loader_forbidden")
		}
	}
	if f.Section(".dynamic") != nil {
		return "", errors.New("elf_dynamic_dependency_forbidden")
	}
	note := f.Section(".note.go.buildid")
	if note == nil {
		return "", errors.New("elf_build_id_missing")
	}
	data, err := note.Data()
	if err != nil || len(data) == 0 {
		return "", errors.New("elf_build_id_missing")
	}
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:]), nil
}

// ValidateManifest is exposed for offline release verification.
func ValidateManifest(m Manifest) error { return validateManifest(m) }
