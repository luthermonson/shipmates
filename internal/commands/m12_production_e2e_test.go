package commands

import (
	"bytes"
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestM12RealCodexPNG is deliberately opt-in because it consumes a real Codex
// turn. Run with SHIPMATES_REAL_CODEX_IMAGE_E2E=1 on an unrestricted host with
// the reviewed Codex 0.144.1 binary authenticated on PATH.
func TestM12RealCodexPNG(t *testing.T) {
	if os.Getenv("SHIPMATES_REAL_CODEX_IMAGE_E2E") != "1" {
		t.Skip("set SHIPMATES_REAL_CODEX_IMAGE_E2E=1 with authenticated Codex 0.144.1 to run the real PNG E2E")
	}
	version, err := exec.Command("codex", "--version").Output()
	if err != nil {
		t.Fatalf("real Codex unavailable: %v", err)
	}
	if strings.TrimSpace(string(version)) != "codex-cli 0.144.1" {
		t.Fatalf("unsupported real Codex generation: %q", version)
	}
	t.Chdir(t.TempDir())
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize production fixture git repository: %v: %s", err, out)
	}
	installCodexPersona(t, "security")
	// Deterministic 1x1 opaque red PNG.
	png, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAusB9Y9Z2SAAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("red.png", png, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err = dispatchToImages(ctx, "security", "Reply with exactly IMAGE_OK after inspecting the attached image.", true, []string{"red.png"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("real Codex PNG turn: %v; progress=%q", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "IMAGE_OK") {
		t.Fatalf("real Codex did not produce deterministic acknowledgement: %q", stdout.String())
	}
}
