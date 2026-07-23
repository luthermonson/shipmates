package installer

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestReleaseDescriptorExtractionIsCanonical(t *testing.T) {
	d, err := ReleaseDescriptorFor()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil || !strings.HasPrefix(string(b), `{"schema":"shipmates.install.descriptor.v1"`) {
		t.Fatalf("descriptor=%s err=%v", b, err)
	}
	if d.Sequence != ReleaseSequence || d.Manifest == "" || len(d.Roles) != 3 {
		t.Fatalf("descriptor=%+v", d)
	}
}

func TestValidateUpgradeRejectsDowngradeEqualChangeAndUnknownSchema(t *testing.T) {
	base, err := ManifestFor()
	if err != nil {
		t.Fatal(err)
	}
	predecessor := ReleaseIdentity{Descriptor: descriptorFor(base), Manifest: base, Pointer: base.Release, Inventory: inventoryForManifest(base)}

	candidate := base
	candidate.Release = "shipmates-runtime-v2"
	candidate.Sequence = 2
	candidate.DescriptorSchema = "shipmates.install.descriptor.v1"
	candidate.DescriptorSchema = base.DescriptorSchema
	candidateDescriptor := descriptorFor(candidate)
	if err := ValidateUpgrade(predecessor, ReleaseIdentity{Descriptor: candidateDescriptor, Manifest: candidate, Pointer: candidate.Release, Inventory: inventoryForManifest(candidate)}); err != nil {
		t.Fatalf("valid upgrade rejected: %v", err)
	}

	if err := ValidateUpgrade(ReleaseIdentity{Descriptor: candidateDescriptor, Manifest: candidate, Pointer: candidate.Release, Inventory: inventoryForManifest(candidate)}, predecessor); err == nil || err.Error() != "release_downgrade" {
		t.Fatalf("downgrade result=%v", err)
	}

	equal := base
	equal.Assets = append([]Asset(nil), base.Assets...)
	equal.Assets[0].Digest = strings.Repeat("a", 64)
	if err := ValidateUpgrade(predecessor, ReleaseIdentity{Descriptor: descriptorFor(equal), Manifest: equal, Pointer: equal.Release, Inventory: inventoryForManifest(equal)}); err == nil || err.Error() != "release_equal_changed" {
		t.Fatalf("equal changed result=%v", err)
	}

	unknown := candidateDescriptor
	unknown.Schema = "shipmates.install.descriptor.v99"
	if err := ValidateUpgrade(predecessor, ReleaseIdentity{Descriptor: unknown, Manifest: candidate, Pointer: candidate.Release, Inventory: inventoryForManifest(candidate)}); err == nil {
		t.Fatal("unknown descriptor accepted")
	}
}
