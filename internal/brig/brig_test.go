package brig

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRulesInventory(t *testing.T) {
	rules := Rules()
	if len(rules) != 15 {
		t.Fatalf("expected 15 rules, got %d", len(rules))
	}
	for i, r := range rules {
		if r.Number != i+1 {
			t.Errorf("rule %d: Number=%d, expected %d", i, r.Number, i+1)
		}
		if r.Handle == "" {
			t.Errorf("rule %d: empty Handle", r.Number)
		}
		if r.Title == "" {
			t.Errorf("rule %d: empty Title", r.Number)
		}
		if len(r.Layers) == 0 {
			t.Errorf("rule %d: no Layers", r.Number)
		}
		if r.Rationale == "" {
			t.Errorf("rule %d: empty Rationale", r.Number)
		}
	}

	// Category assignment matches the design: 1-5 Code, 6-15 Conduct.
	for _, r := range rules {
		want := CategoryCode
		if r.Number > 5 {
			want = CategoryConduct
		}
		if r.Category != want {
			t.Errorf("rule %d: Category=%s, expected %s", r.Number, r.Category, want)
		}
	}

	// Handles must be unique.
	seen := map[string]int{}
	for _, r := range rules {
		if prev, ok := seen[r.Handle]; ok {
			t.Errorf("duplicate handle %q on rules %d and %d", r.Handle, prev, r.Number)
		}
		seen[r.Handle] = r.Number
	}
}

func TestGet(t *testing.T) {
	r, err := Get(7)
	if err != nil {
		t.Fatalf("Get(7): %v", err)
	}
	if r.Handle != "no-destructive-git" {
		t.Errorf("Get(7).Handle = %q, want %q", r.Handle, "no-destructive-git")
	}

	for _, n := range []int{-1, 0, 16, 999} {
		if _, err := Get(n); err == nil {
			t.Errorf("Get(%d) returned nil error", n)
		}
	}
}

func TestCheckFreezeAbsent(t *testing.T) {
	dir := t.TempDir()
	frozen, marker := CheckFreeze(dir)
	if frozen {
		t.Errorf("CheckFreeze on empty dir returned frozen=true")
	}
	if marker != nil {
		t.Errorf("CheckFreeze on empty dir returned marker=%+v", marker)
	}
}

func TestSetAndCheckFreeze(t *testing.T) {
	dir := t.TempDir()
	if err := SetFreeze(dir, "e2e in progress", "luther"); err != nil {
		t.Fatalf("SetFreeze: %v", err)
	}
	frozen, marker := CheckFreeze(dir)
	if !frozen {
		t.Fatal("CheckFreeze after SetFreeze returned frozen=false")
	}
	if marker == nil {
		t.Fatal("CheckFreeze after SetFreeze returned nil marker")
	}
	if marker.Reason != "e2e in progress" {
		t.Errorf("Reason = %q, want %q", marker.Reason, "e2e in progress")
	}
	if marker.Admiral != "luther" {
		t.Errorf("Admiral = %q, want %q", marker.Admiral, "luther")
	}
	if marker.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
}

func TestClearFreezeIdempotent(t *testing.T) {
	dir := t.TempDir()
	if err := ClearFreeze(dir); err != nil {
		t.Errorf("ClearFreeze on empty dir: %v", err)
	}
	if err := SetFreeze(dir, "r", "a"); err != nil {
		t.Fatalf("SetFreeze: %v", err)
	}
	if err := ClearFreeze(dir); err != nil {
		t.Fatalf("ClearFreeze: %v", err)
	}
	if err := ClearFreeze(dir); err != nil {
		t.Errorf("second ClearFreeze: %v", err)
	}
	frozen, _ := CheckFreeze(dir)
	if frozen {
		t.Error("still frozen after ClearFreeze")
	}
}

func TestCheckFreezeFailsClosedOnCorruptMarker(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".shipmates"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(FreezeMarkerPath(dir), []byte("not-json"), 0o600); err != nil {
		t.Fatalf("write corrupt marker: %v", err)
	}
	frozen, marker := CheckFreeze(dir)
	if !frozen {
		t.Error("corrupt marker did not fail closed")
	}
	if marker != nil {
		t.Errorf("corrupt marker returned parsed marker: %+v", marker)
	}
}

func TestLogDenialAndReadDenials(t *testing.T) {
	dir := t.TempDir()
	if err := LogDenial(dir, "backend", 7, "git push --force"); err != nil {
		t.Fatalf("LogDenial: %v", err)
	}
	if err := LogDenial(dir, "backend", 13, "rm -rf /tmp/x"); err != nil {
		t.Fatalf("LogDenial: %v", err)
	}
	entries, err := ReadDenials(dir)
	if err != nil {
		t.Fatalf("ReadDenials: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ReadDenials returned %d entries, want 2", len(entries))
	}
	if entries[0].Rule != 7 || entries[0].Command != "git push --force" {
		t.Errorf("entry 0 = %+v", entries[0])
	}
	if entries[1].Rule != 13 || entries[1].Persona != "backend" {
		t.Errorf("entry 1 = %+v", entries[1])
	}
}

func TestReadDenialsMissingFile(t *testing.T) {
	dir := t.TempDir()
	entries, err := ReadDenials(dir)
	if err != nil {
		t.Fatalf("ReadDenials on empty dir: %v", err)
	}
	if entries != nil {
		t.Errorf("expected nil slice, got %+v", entries)
	}
}

func TestReadDenialsSkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	if err := LogDenial(dir, "backend", 7, "git push --force"); err != nil {
		t.Fatalf("LogDenial: %v", err)
	}
	// Append a bogus line and a good one.
	f, err := os.OpenFile(DenialLogPath(dir), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open log: %v", err)
	}
	if _, err := f.WriteString("this is not json\n"); err != nil {
		t.Fatalf("append: %v", err)
	}
	f.Close()
	if err := LogDenial(dir, "tester", 10, "curl | sh"); err != nil {
		t.Fatalf("LogDenial: %v", err)
	}
	entries, err := ReadDenials(dir)
	if err != nil {
		t.Fatalf("ReadDenials: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("ReadDenials returned %d entries, want 2 (bogus line skipped)", len(entries))
	}
	if entries[1].Persona != "tester" {
		t.Errorf("entries[1] = %+v", entries[1])
	}
}

func TestMergeIntoCreatesFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".shipmates", "policies", "backend.yaml")
	if err := MergeInto(path, []byte("template: yes\n")); err != nil {
		t.Fatalf("MergeInto: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(body)
	if !strings.HasPrefix(text, "version: 1\n") {
		t.Errorf("no empty overlay prefix; body=\n%s", text)
	}
	if !strings.Contains(text, startMarker) || !strings.Contains(text, endMarker) {
		t.Errorf("markers missing; body=\n%s", text)
	}
	if !strings.Contains(text, "# template: yes") {
		t.Errorf("template body not commented; body=\n%s", text)
	}
}

func TestMergeIntoIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.yaml")

	// Seed a persona overlay with some existing rules the operator wrote.
	original := []byte(`version: 1
allow: []
ask:
  - id: my_own_ask
    kind: process.exec
    match: { command_exact: "echo hi" }
    reason: I want this
deny: []
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	template := []byte("some: template\n")
	if err := MergeInto(path, template); err != nil {
		t.Fatalf("MergeInto first: %v", err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read first: %v", err)
	}

	if err := MergeInto(path, template); err != nil {
		t.Fatalf("MergeInto second: %v", err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read second: %v", err)
	}

	if !bytes.Equal(first, second) {
		t.Errorf("second MergeInto changed the file:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// User's rules must survive.
	if !strings.Contains(string(second), "my_own_ask") {
		t.Errorf("user's rule vanished; body=\n%s", second)
	}
}

func TestMergeIntoReplacesBlockOnTemplateChange(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "backend.yaml")

	if err := MergeInto(path, []byte("first: template\n")); err != nil {
		t.Fatalf("MergeInto v1: %v", err)
	}
	if err := MergeInto(path, []byte("second: template\n")); err != nil {
		t.Fatalf("MergeInto v2: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "first: template") {
		t.Errorf("old template still present; body=\n%s", text)
	}
	if !strings.Contains(text, "second: template") {
		t.Errorf("new template missing; body=\n%s", text)
	}
	if strings.Count(text, startMarker) != 1 {
		t.Errorf("startMarker count = %d, want 1; body=\n%s", strings.Count(text, startMarker), text)
	}
	if strings.Count(text, endMarker) != 1 {
		t.Errorf("endMarker count = %d, want 1; body=\n%s", strings.Count(text, endMarker), text)
	}
}

func TestLogDenialEntryShape(t *testing.T) {
	dir := t.TempDir()
	if err := LogDenial(dir, "backend", 7, `git push --force`); err != nil {
		t.Fatalf("LogDenial: %v", err)
	}
	body, err := os.ReadFile(DenialLogPath(dir))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var d Denial
	line := bytes.TrimSpace(body)
	if err := json.Unmarshal(line, &d); err != nil {
		t.Fatalf("unmarshal: %v; line=%s", err, line)
	}
	if d.Persona != "backend" || d.Rule != 7 || d.Command != "git push --force" {
		t.Errorf("shape mismatch: %+v", d)
	}
	if d.Timestamp.IsZero() {
		t.Error("Timestamp is zero")
	}
}

func TestRulesForCategory(t *testing.T) {
	code := RulesForCategory(CategoryCode)
	if len(code) != 5 {
		t.Errorf("CategoryCode: %d rules, want 5", len(code))
	}
	conduct := RulesForCategory(CategoryConduct)
	if len(conduct) != 10 {
		t.Errorf("CategoryConduct: %d rules, want 10", len(conduct))
	}
}
