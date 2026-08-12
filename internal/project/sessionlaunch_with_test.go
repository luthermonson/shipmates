package project

import (
	"reflect"
	"testing"
)

// SessionLaunchWith is the seam sail uses to dispatch a crew turn at a
// specific escalation tier: the caller supplies the config instead of having
// it resolved from the persona file, and the fingerprint logic must behave
// exactly as it does for SessionLaunch.
func TestSessionLaunchWithResumesOnMatchingFingerprint(t *testing.T) {
	t.Chdir(t.TempDir())
	cfg := PersonaConfig{Model: "claude-opus-4-7", Effort: "low"}
	if err := WriteSessionMeta("backend", "backend", "uuid-tier", cfg.Fingerprint(), ""); err != nil {
		t.Fatal(err)
	}
	args, id, _, fp, creating := SessionLaunchWith("backend", cfg, false)
	if creating || id != "uuid-tier" || fp != cfg.Fingerprint() {
		t.Fatalf("creating=%v id=%q fp=%q", creating, id, fp)
	}
	if want := []string{"--resume", "uuid-tier", "--agent", "backend"}; !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
}

// A tier override whose model/effort differ from the stored session must mint
// a fresh session: model and effort are baked into a claude session at
// creation and cannot change on resume. This is what makes sail's escalation
// ladder real rather than a label.
func TestSessionLaunchWithMintsFreshOnTierChange(t *testing.T) {
	t.Chdir(t.TempDir())
	base := PersonaConfig{Model: "claude-opus-4-7", Effort: "low"}
	if err := WriteSessionMeta("backend", "backend", "uuid-tier", base.Fingerprint(), ""); err != nil {
		t.Fatal(err)
	}
	escalated := PersonaConfig{Model: "claude-opus-4-7", Effort: "high"}
	args, id, _, fp, creating := SessionLaunchWith("backend", escalated, false)
	if !creating {
		t.Fatal("escalated tier resumed a session baked with the old effort")
	}
	if id == "uuid-tier" {
		t.Fatal("escalated tier reused the old session id")
	}
	if fp != escalated.Fingerprint() || fp == base.Fingerprint() {
		t.Fatalf("fp = %q, want the escalated tier's fingerprint", fp)
	}
	if args[0] != "--session-id" {
		t.Fatalf("args = %v, want a --session-id create", args)
	}
}
