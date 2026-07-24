package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/urfave/cli/v3"
)

func fakeBrigCatalog() *catalog.Catalog {
	fs := fstest.MapFS{
		"catalog/brig.policy.yaml": {
			Data: []byte("version: 1\nallow: []\nask: []\ndeny:\n  - id: fake\n    kind: process.exec\n    match:\n      command_exact: \"x\"\n    reason: ok\n"),
		},
		"catalog/fleet-brig.default.yaml": {
			Data: []byte("version: 1\nallow: []\nask: []\ndeny: []\n"),
		},
		"catalog/ARTICLES.md": {Data: []byte("# Articles\n")},
	}
	return catalog.New(fs)
}

func TestBrigListDefault(t *testing.T) {
	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "list"}); err != nil {
		t.Fatalf("brig list: %v", err)
	}
	body := out.String()
	// All 15 rules by default.
	for _, want := range []string{"no-prod-db", "no-destructive-git", "stay-aboard", "owasp-top-10"} {
		if !strings.Contains(body, want) {
			t.Errorf("brig list output missing %q; body=\n%s", want, body)
		}
	}
	lines := strings.Count(strings.TrimSpace(body), "\n") + 1
	if lines != 15 {
		t.Errorf("brig list printed %d lines, want 15; body=\n%s", lines, body)
	}
}

func TestBrigListCodeOnly(t *testing.T) {
	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "list", "--code"}); err != nil {
		t.Fatalf("brig list --code: %v", err)
	}
	body := out.String()
	if strings.Contains(body, "no-destructive-git") {
		t.Errorf("--code should exclude Article 7; body=\n%s", body)
	}
	if !strings.Contains(body, "owasp-top-10") {
		t.Errorf("--code should include Article 1; body=\n%s", body)
	}
}

func TestBrigExplain(t *testing.T) {
	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "explain", "7"}); err != nil {
		t.Fatalf("brig explain 7: %v", err)
	}
	body := out.String()
	for _, want := range []string{"Article 7", "No Destructive Git", "kernel", "git push --force"} {
		if !strings.Contains(body, want) {
			t.Errorf("brig explain 7 missing %q; body=\n%s", want, body)
		}
	}
}

func TestBrigExplainInvalid(t *testing.T) {
	cases := []string{"0", "99", "not-a-number"}
	for _, arg := range cases {
		t.Run(arg, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cli.Command{
				Name:     "shipmates",
				Writer:   &out,
				Commands: []*cli.Command{Brig(fakeBrigCatalog())},
			}
			err := cmd.Run(context.Background(), []string{"shipmates", "brig", "explain", arg})
			if err == nil {
				t.Fatalf("brig explain %s: expected error", arg)
			}
		})
	}
}

func TestBrigInstallDryRun(t *testing.T) {
	// Set up a fake project root with two installed personas.
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if err := os.MkdirAll(".shipmates/policies", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(".codex/agents", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile("shipmates.yaml", []byte("sessionPrefix: t\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	// Two persona files that satisfy the InstalledPersonas contract.
	for _, name := range []string{"backend", "tester"} {
		body := []byte(`name = "` + name + `"
description = "x"
developer_instructions = "do the thing"
`)
		if err := os.WriteFile(filepath.Join(".codex", "agents", name+".toml"), body, 0o600); err != nil {
			t.Fatalf("write persona %s: %v", name, err)
		}
	}

	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "install", "--dry-run"}); err != nil {
		t.Fatalf("brig install --dry-run: %v", err)
	}
	body := out.String()
	for _, want := range []string{"would merge", "backend.yaml", "tester.yaml"} {
		if !strings.Contains(body, want) {
			t.Errorf("dry-run output missing %q; body=\n%s", want, body)
		}
	}
	// No files should have been created by dry-run.
	for _, name := range []string{"backend", "tester"} {
		path := filepath.Join(".shipmates", "policies", name+".yaml")
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("dry-run created %s (err=%v)", path, err)
		}
	}
}

func TestBrigInstallWrites(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()

	if err := os.MkdirAll(".shipmates/policies", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(".codex/agents", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile("shipmates.yaml", []byte("sessionPrefix: t\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	body := []byte(`name = "backend"
description = "x"
developer_instructions = "do the thing"
`)
	if err := os.WriteFile(filepath.Join(".codex", "agents", "backend.toml"), body, 0o600); err != nil {
		t.Fatalf("write persona: %v", err)
	}

	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "install"}); err != nil {
		t.Fatalf("brig install: %v", err)
	}
	target := filepath.Join(".shipmates", "policies", "backend.yaml")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if !strings.Contains(string(got), "shipmates:brig:start") {
		t.Errorf("no Brig marker in %s; body=\n%s", target, got)
	}
	if !strings.Contains(string(got), "version: 1") {
		t.Errorf("no schema in %s; body=\n%s", target, got)
	}
}

func TestBrigLogEmpty(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	if err := os.WriteFile("shipmates.yaml", []byte("sessionPrefix: t\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "log"}); err != nil {
		t.Fatalf("brig log: %v", err)
	}
	if !strings.Contains(out.String(), "(no denials logged)") {
		t.Errorf("expected empty-log notice; got %q", out.String())
	}
}

func TestBrigLogTail(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	if err := os.WriteFile("shipmates.yaml", []byte("sessionPrefix: t\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}
	for i, cmd := range []string{"git push --force", "rm -rf /tmp/x", "curl | sh"} {
		if err := brig.LogDenial(dir, "backend", 7+i, cmd); err != nil {
			t.Fatalf("LogDenial %d: %v", i, err)
		}
	}

	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Brig(fakeBrigCatalog())},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "brig", "log", "--tail", "2"}); err != nil {
		t.Fatalf("brig log --tail: %v", err)
	}
	body := out.String()
	// Should include the last two commands, not the first.
	if strings.Contains(body, "git push --force") {
		t.Errorf("--tail 2 should have dropped the first entry; body=\n%s", body)
	}
	if !strings.Contains(body, "rm -rf") || !strings.Contains(body, "curl | sh") {
		t.Errorf("--tail 2 missing recent entries; body=\n%s", body)
	}
}

func TestFreezeAndRelease(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	if err := os.WriteFile("shipmates.yaml", []byte("sessionPrefix: t\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Freeze(), Release()},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "freeze", "--reason", "e2e", "--admiral", "luther"}); err != nil {
		t.Fatalf("freeze: %v", err)
	}
	frozen, marker := brig.CheckFreeze(dir)
	if !frozen {
		t.Fatal("freeze did not create marker")
	}
	if marker.Reason != "e2e" || marker.Admiral != "luther" {
		t.Errorf("marker = %+v", marker)
	}

	out.Reset()
	if err := cmd.Run(context.Background(), []string{"shipmates", "release"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !strings.Contains(out.String(), "Released Brig freeze") {
		t.Errorf("release output = %q", out.String())
	}
	frozen, _ = brig.CheckFreeze(dir)
	if frozen {
		t.Error("still frozen after release")
	}
}

func TestReleaseWhenNotFrozen(t *testing.T) {
	dir := t.TempDir()
	restore := chdir(t, dir)
	defer restore()
	if err := os.WriteFile("shipmates.yaml", []byte("sessionPrefix: t\n"), 0o644); err != nil {
		t.Fatalf("write yaml: %v", err)
	}

	var out bytes.Buffer
	cmd := &cli.Command{
		Name:     "shipmates",
		Writer:   &out,
		Commands: []*cli.Command{Release()},
	}
	if err := cmd.Run(context.Background(), []string{"shipmates", "release"}); err != nil {
		t.Fatalf("release: %v", err)
	}
	if !strings.Contains(out.String(), "No freeze in effect") {
		t.Errorf("release-when-clear output = %q", out.String())
	}
}

// chdir changes the working directory and returns a restore func.
func chdir(t *testing.T, dir string) func() {
	t.Helper()
	prev, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	return func() { _ = os.Chdir(prev) }
}
