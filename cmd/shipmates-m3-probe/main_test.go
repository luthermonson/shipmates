//go:build linux

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProbeControlParsingAndProtectedInputs(t *testing.T) {
	if !containsPopulated([]byte("populated 1\n frozen 0\n"), "1") || containsPopulated([]byte("populated 0\n"), "1") {
		t.Fatal("populated transition parsing is incorrect")
	}
	dir := t.TempDir()
	config := filepath.Join(dir, "fleet.yaml")
	if err := os.WriteFile(config, []byte("tls: protected\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := protectedFile(config); err != nil {
		t.Fatal(err)
	}
	if err := evidenceDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := protectedFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing config accepted")
	}
	link := filepath.Join(dir, "config-link")
	if err := os.Symlink(config, link); err != nil {
		t.Fatal(err)
	}
	if err := protectedFile(link); err == nil {
		t.Fatal("config symlink accepted")
	}
	if err := writeEvidence(dir, map[string]any{"event": "delegated_cgroup_probe", "result": "pass"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "m3-cgroup-probe.json")); err != nil {
		t.Fatal(err)
	}
	if err := writeEvidence(dir, map[string]any{"event": "replacement"}); err == nil {
		t.Fatal("existing evidence file was replaced")
	}
}
