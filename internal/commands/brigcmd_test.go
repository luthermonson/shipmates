package commands

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/brig"
)

func TestBrigListCommand(t *testing.T) {
	pinHome(t)
	var out bytes.Buffer
	cmd := Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "list"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"no-destructive-git", "owasp-top-10", "respect-the-freeze", "No Prod DB"} {
		if !strings.Contains(s, want) {
			t.Errorf("brig list missing %q:\n%s", want, s)
		}
	}
	if got := strings.Count(s, "\n"); got != 15 {
		t.Errorf("brig list printed %d lines, want 15", got)
	}

	// --conduct narrows to the ten conduct Articles.
	out.Reset()
	cmd = Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "list", "--conduct"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(out.String(), "\n"); got != 10 {
		t.Errorf("brig list --conduct printed %d lines, want 10", got)
	}
}

func TestBrigListMarksWaivedArticles(t *testing.T) {
	home := pinHome(t)
	dir := filepath.Join(home, ".shipmates")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	conf := "brig:\n  disabled_articles: [twelve-factor]\n"
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(conf), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "list"}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, "twelve-factor") && !strings.Contains(line, "waived") {
			t.Errorf("waived Article not marked: %q", line)
		}
		if strings.Contains(line, "no-destructive-git") && strings.Contains(line, "waived") {
			t.Errorf("in-force Article marked waived: %q", line)
		}
	}
}

func TestBrigExplainCommand(t *testing.T) {
	pinHome(t)
	var out bytes.Buffer
	cmd := Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "explain", "14"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	for _, want := range []string{"Article 14", "no-self-escalation", "fleet-wide deny"} {
		if !strings.Contains(s, want) {
			t.Errorf("explain 14 missing %q:\n%s", want, s)
		}
	}
	if err := Brig().Run(context.Background(), []string{"brig", "explain", "99"}); err == nil {
		t.Error("explain 99 should error")
	}
	if err := Brig().Run(context.Background(), []string{"brig", "explain", "seven"}); err == nil {
		t.Error("explain seven should error")
	}
}

func TestBrigLogCommand(t *testing.T) {
	pinHome(t)
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	cmd := Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "log"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "(no denials logged)") {
		t.Errorf("empty log output = %q", out.String())
	}

	for i, cmdline := range []string{"git push --force", "curl | sh", "rm -rf /"} {
		if err := brig.LogDenial(".", "backend", 7+i, cmdline); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	cmd = Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "log", "--tail", "2"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if strings.Contains(s, "git push --force") {
		t.Errorf("--tail 2 shows the oldest entry:\n%s", s)
	}
	if !strings.Contains(s, "curl | sh") || !strings.Contains(s, "rm -rf /") {
		t.Errorf("--tail 2 missing recent entries:\n%s", s)
	}
}

func TestFreezeAndReleaseCommands(t *testing.T) {
	pinHome(t)
	t.Chdir(t.TempDir())

	var out bytes.Buffer
	cmd := Freeze()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"freeze", "--reason", "weird diff", "--admiral", "luther"}); err != nil {
		t.Fatal(err)
	}
	frozen, marker := brig.CheckFreeze(".")
	if !frozen || marker == nil || marker.Reason != "weird diff" || marker.Admiral != "luther" {
		t.Fatalf("freeze command did not engage: %v %+v", frozen, marker)
	}
	if !strings.Contains(out.String(), "engaged") {
		t.Errorf("freeze output = %q", out.String())
	}

	out.Reset()
	rel := Release()
	rel.Writer = &out
	if err := rel.Run(context.Background(), []string{"release"}); err != nil {
		t.Fatal(err)
	}
	if frozen, _ := brig.CheckFreeze("."); frozen {
		t.Fatal("release did not clear the marker")
	}
	if !strings.Contains(out.String(), "weird diff") {
		t.Errorf("release output should recap the marker: %q", out.String())
	}

	// Releasing again is idempotent.
	out.Reset()
	rel = Release()
	rel.Writer = &out
	if err := rel.Run(context.Background(), []string{"release"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "No freeze in effect") {
		t.Errorf("second release output = %q", out.String())
	}
}

// TestFreezeRefusedWhenBrigDisabled: engaging a stop nothing enforces would
// be worse than no stop; the command says so instead. Release still works —
// clearing state is never gated.
func TestFreezeRefusedWhenBrigDisabled(t *testing.T) {
	home := pinHome(t)
	disableBrig(t, home)
	t.Chdir(t.TempDir())

	err := Freeze().Run(context.Background(), []string{"freeze", "--reason", "x"})
	if err == nil {
		t.Fatal("freeze should refuse while the brig is disabled")
	}
	if !strings.Contains(err.Error(), "disabled") {
		t.Errorf("refusal should say why: %v", err)
	}
	if frozen, _ := brig.CheckFreeze("."); frozen {
		t.Fatal("refused freeze still wrote a marker")
	}

	// A marker engaged before disabling can still be released.
	if err := brig.SetFreeze(".", "old", "x"); err != nil {
		t.Fatal(err)
	}
	if err := Release().Run(context.Background(), []string{"release"}); err != nil {
		t.Fatalf("release must work with the brig disabled: %v", err)
	}
	if frozen, _ := brig.CheckFreeze("."); frozen {
		t.Fatal("release with brig disabled did not clear the marker")
	}
}

func TestBrigStatusCommand(t *testing.T) {
	pinHome(t)
	t.Chdir(t.TempDir())
	var out bytes.Buffer
	cmd := Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "status"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "brig: enabled") || !strings.Contains(s, "freeze: not engaged") {
		t.Errorf("default status = %q", s)
	}

	if err := brig.SetFreeze(".", "drill", "luther"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	cmd = Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "status"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ENGAGED") || !strings.Contains(out.String(), "drill") {
		t.Errorf("frozen status = %q", out.String())
	}
}

func TestBrigStatusDisabledWithSuspendedFreeze(t *testing.T) {
	home := pinHome(t)
	disableBrig(t, home)
	t.Chdir(t.TempDir())
	if err := brig.SetFreeze(".", "before disabling", "x"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd := Brig()
	cmd.Writer = &out
	if err := cmd.Run(context.Background(), []string{"brig", "status"}); err != nil {
		t.Fatal(err)
	}
	s := out.String()
	if !strings.Contains(s, "DISABLED") {
		t.Errorf("status hides the disabled posture: %q", s)
	}
	if !strings.Contains(s, "NOT enforced") {
		t.Errorf("status hides the suspended freeze marker: %q", s)
	}
}

