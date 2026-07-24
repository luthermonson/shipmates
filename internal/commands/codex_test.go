package commands

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/turninput"
)

// skipIfNoPOSIXShell skips tests that install a `#!/bin/sh` fake codex
// binary and then expect the OS to execute it. Windows has no POSIX shell
// interpreter, so shell-based fakes never load via exec.LookPath.
func skipIfNoPOSIXShell(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake codex fixture is a #!/bin/sh script; Windows cannot exec it")
	}
}

func TestCodexArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll(filepath.Dir(project.CodexAgentPath("security")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.CodexAgentPath("security"), []byte("name = \"security\"\ndeveloper_instructions = \"# Role\\n\\nReview carefully.\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fresh, err := codexArgs("security", "check this", true, "", project.PersonaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fresh[:4], " "); got != "exec --json --sandbox workspace-write" {
		t.Fatalf("fresh args = %q", got)
	}
	if !strings.Contains(fresh[len(fresh)-1], ".shipmates/memory/security/") {
		t.Fatalf("fresh prompt does not preserve memory: %q", fresh[len(fresh)-1])
	}
	configured, err := codexArgs("security", "configured", true, "", project.PersonaConfig{Model: "small-model", Effort: "low"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(configured, " ")
	if !strings.Contains(joined, "--model small-model") || !strings.Contains(joined, `--config model_reasoning_effort="low"`) {
		t.Fatalf("model and effort were not applied: %q", configured)
	}

	resume, err := codexArgs("security", "continue", false, "thread-123", project.PersonaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(resume, " "); got != "exec resume thread-123 --json continue" {
		t.Fatalf("resume args = %q", got)
	}
}

func installFakeCodex(t *testing.T, script string) {
	t.Helper()
	useLegacyCodexTestDispatcher(t)
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nset -eu\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin)+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func useLegacyCodexTestDispatcher(t *testing.T) {
	t.Helper()
	previous := codexTurnDispatcher
	codexTurnDispatcher = func(ctx context.Context, installed *project.InstalledPersona, prompt string, fresh bool, cfg project.PersonaConfig, images []turninput.ImageDescriptorV1, stdout, stderr io.Writer) error {
		var batch *turninput.ImageBatchV1
		if len(images) > 0 {
			validated, err := turninput.ValidateImages(mustCanonicalRoot(t), descriptorPaths(images))
			if err != nil {
				return err
			}
			batch = validated
		}
		return dispatchCodexExecInstalledImages(ctx, installed, prompt, fresh, cfg, batch, stdout, stderr)
	}
	t.Cleanup(func() { codexTurnDispatcher = previous })
}

func descriptorPaths(images []turninput.ImageDescriptorV1) []string {
	paths := make([]string, 0, len(images))
	for _, image := range images {
		paths = append(paths, image.DisplayPath())
	}
	return paths
}

func installCodexPersona(t *testing.T, persona string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(project.CodexAgentPath(persona)), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "name = " + strconv.Quote(persona) + "\ndeveloper_instructions = \"Review carefully.\"\n"
	if err := os.WriteFile(project.CodexAgentPath(persona), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(".shipmates", "policies"), 0o755); err != nil {
		t.Fatal(err)
	}
	empty := []byte("version: 1\nallow: []\nask: []\ndeny: []\n")
	if err := os.WriteFile(filepath.Join(".shipmates", "policy.yaml"), empty, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(".shipmates", "policies", persona+".yaml"), empty, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDispatchCodexFreshThenResumeWithoutLegacyRuntime(t *testing.T) {
	skipIfNoPOSIXShell(t)
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	logPath := filepath.Join(t.TempDir(), "argv.log")
	t.Setenv("FAKE_CODEX_LOG", logPath)
	installFakeCodex(t, `printf '%s\n' "$*" >> "$FAKE_CODEX_LOG"
printf '%s\n' '{"type":"thread.started","thread_id":"thread-123"}'
printf '%s\n' '{"type":"item.started","item":{"type":"command_execution","command":"secret"}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"crew answer"}}'`)

	var stdout, stderr bytes.Buffer
	if err := dispatchCodex(context.Background(), "security", "deliver review", false, project.PersonaConfig{}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	if stdout.String() != "crew answer\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if got := stderr.String(); !strings.Contains(got, "started:") || !strings.Contains(got, "activity: running a command") || !strings.Contains(got, "completed:") || strings.Contains(got, "secret") {
		t.Fatalf("unsafe or incomplete progress: %q", got)
	}
	stdout.Reset()
	stderr.Reset()
	if err := dispatchCodex(context.Background(), "security", "follow up", false, project.PersonaConfig{}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "exec resume thread-123 --json follow up") {
		t.Fatalf("resume argv missing: %s", log)
	}
	if _, err := os.Stat(filepath.Join(".legacy-runtime", "agents", "security.md")); !os.IsNotExist(err) {
		t.Fatalf("test unexpectedly used LegacyRuntime artifact: %v", err)
	}
}

func TestDispatchCodexTimeoutPreservesMarker(t *testing.T) {
	skipIfNoPOSIXShell(t)
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	if err := project.WriteBackendSessionMeta("security", "codex", "old", "thread-old", "hash"); err != nil {
		t.Fatal(err)
	}
	installFakeCodex(t, `exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	err := dispatchCodex(ctx, "security", "wait", false, project.PersonaConfig{}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timeout error = %v", err)
	}
	meta, ok := project.ReadBackendSessionMeta("security", "codex")
	if !ok || meta.ID != "thread-old" {
		t.Fatalf("marker replaced: %#v, %v", meta, ok)
	}
	if _, err := os.Stat(filepath.Join(project.SessionsDir(), "security.dispatch.lock")); !os.IsNotExist(err) {
		t.Fatalf("timeout left dispatch lock: %v", err)
	}
}

func TestFreshInitAddAndAskAreCodexNative(t *testing.T) {
	// Init calls withPolicyWriteLock (unix flock), and the persona turn uses
	// a #!/bin/sh fake codex; both fixtures are unix-only.
	skipIfNoPolicyLock(t)
	skipIfNoPOSIXShell(t)
	t.Chdir(t.TempDir())
	cat := catalog.New(fstest.MapFS{
		"catalog/security/agent.md": {Data: []byte("---\nname: security\ndescription: Security review.\n---\n\nReview security carefully.\n")},
	})
	if err := Init(cat).Run(context.Background(), []string{"shipmates", "--crew", "security"}); err != nil {
		t.Fatalf("init --crew security: %v", err)
	}
	if _, err := os.Stat(".legacy-runtime"); !os.IsNotExist(err) {
		t.Fatalf("fresh init created .legacy-runtime: %v", err)
	}
	installFakeCodex(t, `printf '%s\n' '{"type":"thread.started","thread_id":"thread-clean-room"}'
printf '%s\n' '{"type":"item.started","item":{"type":"command_execution"}}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"clean-room answer"}}'`)

	var stdout, stderr bytes.Buffer
	if err := dispatchTo(context.Background(), "security", "return the review", false, &stdout, &stderr); err != nil {
		t.Fatalf("ask security in Codex-native project: %v", err)
	}
	if stdout.String() != "clean-room answer\n" {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if _, err := os.Stat(project.CodexAgentPath("security")); err != nil {
		t.Fatalf("canonical Codex artifact missing: %v", err)
	}
	for _, path := range []string{filepath.Join(".shipmates", "policy.yaml"), project.PolicyPath("security")} {
		b, err := os.ReadFile(path)
		if err != nil || string(b) != emptyStrictPolicy {
			t.Fatalf("required policy %s = %q, %v", path, b, err)
		}
	}
	if _, ok := project.ReadBackendSessionMeta("security", "codex"); !ok {
		t.Fatal("Codex session marker not recorded")
	}
}

func TestParseCodexEvent(t *testing.T) {
	id, text, err, _ := parseCodexEvent([]byte(`{"type":"thread.started","thread_id":"thread-123"}`))
	if id != "thread-123" || text != "" || err != "" {
		t.Fatalf("thread event = %q, %q, %q", id, text, err)
	}
	_, text, err, _ = parseCodexEvent([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"done"}}`))
	if text != "done" || err != "" {
		t.Fatalf("item event = %q, %q", text, err)
	}
}

func TestCodexImageArgsFreshResumeOrderAndZeroImageIdentity(t *testing.T) {
	t.Chdir(t.TempDir())
	installCodexPersona(t, "security")
	paths := []string{"space image.png", "-leading.png", "雪'$.gif"}
	for i, path := range paths {
		header := [][]byte{{0x89, 'P', 'N', 'G', 13, 10, 26, 10}, {0xff, 0xd8, 0xff, 0xe0}, []byte("GIF89a")}[i]
		if err := os.WriteFile(path, header, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := project.CanonicalRoot(".")
	batch, err := turninput.ValidateImages(root, paths)
	if err != nil {
		t.Fatal(err)
	}
	defer batch.Close()
	installed, err := project.CanonicalPersonaAt(".", "security")
	if err != nil {
		t.Fatal(err)
	}
	images := batch.Images()
	fresh, err := codexArgsInstalledImages(installed, "inspect", true, "", project.PersonaConfig{}, images)
	if err != nil {
		t.Fatal(err)
	}
	wantFreshPrefix := []string{"exec", "--json", "--sandbox", "workspace-write"}
	if !reflect.DeepEqual(fresh[:4], wantFreshPrefix) {
		t.Fatalf("fresh prefix=%q", fresh)
	}
	for i, image := range images {
		at := 4 + i*2
		if fresh[at] != "--image" || fresh[at+1] != image.AbsolutePath() {
			t.Fatalf("fresh image order=%q", fresh)
		}
	}
	if fresh[len(fresh)-2] != "--" {
		t.Fatalf("fresh prompt separator missing: %q", fresh)
	}
	if strings.Contains(fresh[len(fresh)-1], paths[0]) || !strings.Contains(fresh[len(fresh)-1], "inspect") {
		t.Fatalf("image leaked into prompt=%q", fresh[len(fresh)-1])
	}
	resume, err := codexArgsInstalledImages(installed, "continue", false, "thread-1", project.PersonaConfig{}, images)
	if err != nil {
		t.Fatal(err)
	}
	wantResume := []string{"exec", "resume", "thread-1", "--json"}
	for _, image := range images {
		wantResume = append(wantResume, "--image", image.AbsolutePath())
	}
	wantResume = append(wantResume, "--", "continue")
	if !reflect.DeepEqual(resume, wantResume) {
		t.Fatalf("resume=%q want=%q", resume, wantResume)
	}
	zero, _ := codexArgsInstalledImages(installed, "plain", false, "thread-1", project.PersonaConfig{}, nil)
	legacy, _ := codexArgsInstalled(installed, "plain", false, "thread-1", project.PersonaConfig{})
	if !reflect.DeepEqual(zero, legacy) {
		t.Fatalf("zero-image argv changed: %q != %q", zero, legacy)
	}
}

func TestAskImageProcessRefusalFailureAndMarkerPreservation(t *testing.T) {
	t.Run("invalid-no-child", func(t *testing.T) {
		t.Chdir(t.TempDir())
		installCodexPersona(t, "security")
		marker := filepath.Join(t.TempDir(), "child-started")
		if err := project.WriteBackendSessionMeta("security", "codex", "old", "thread-old", "hash"); err != nil {
			t.Fatal(err)
		}
		err := dispatchImages(context.Background(), "security", "inspect", false, []string{"../secret.png"})
		if err == nil {
			t.Fatalf("error=%v", err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("invalid input started child: %v", err)
		}
		meta, ok := project.ReadBackendSessionMeta("security", "codex")
		if !ok || meta.ID != "thread-old" {
			t.Fatalf("marker changed=%+v", meta)
		}
	})
}

func TestAskImageFakeCodexExactArgvAndCrashCancellation(t *testing.T) {
	skipIfNoPOSIXShell(t)
	for _, mode := range []string{"success", "crash", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			t.Chdir(t.TempDir())
			installCodexPersona(t, "security")
			for name, raw := range map[string][]byte{"space image.png": {0x89, 'P', 'N', 'G', 13, 10, 26, 10}, "-雪.webp": []byte("RIFF0000WEBPVP8X")} {
				if err := os.WriteFile(name, raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			logPath := filepath.Join(t.TempDir(), "argv")
			t.Setenv("FAKE_CODEX_LOG", logPath)
			body := `if [ "$1" = exec ] && [ "$2" = --help ]; then printf '%s\n' '--image'; exit 0; fi
printf '<%s>\n' "$@" >> "$FAKE_CODEX_LOG"
`
			switch mode {
			case "success":
				body += `printf '%s\n' '{"type":"thread.started","thread_id":"thread-image"}' '{"type":"item.completed","item":{"type":"agent_message","text":"image-ok"}}'`
			case "crash":
				body += `exit 9`
			case "cancel":
				body += `exec sleep 30`
			}
			installFakeCodex(t, body)
			if err := project.WriteBackendSessionMeta("security", "codex", "old", "thread-old", "hash"); err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer cancel()
			}
			var out, diag bytes.Buffer
			err := dispatchToImages(ctx, "security", "inspect", true, []string{"space image.png", "-雪.webp"}, &out, &diag)
			if mode == "success" {
				if err != nil || out.String() != "image-ok\n" {
					t.Fatalf("success err=%v out=%q", err, out.String())
				}
				log, _ := os.ReadFile(logPath)
				if strings.Count(string(log), "<--image>") != 2 || !strings.Contains(string(log), "<"+filepath.Join(mustCanonicalRoot(t), "space image.png")+">") || !strings.Contains(string(log), "<"+filepath.Join(mustCanonicalRoot(t), "-雪.webp")+">") {
					t.Fatalf("argv boundaries=%q", log)
				}
			} else if err == nil {
				t.Fatalf("%s unexpectedly succeeded", mode)
			}
			if mode != "success" {
				meta, ok := project.ReadBackendSessionMeta("security", "codex")
				if !ok || meta.ID != "thread-old" {
					t.Fatalf("failure changed marker=%+v", meta)
				}
			}
		})
	}
}

func mustCanonicalRoot(t *testing.T) string {
	t.Helper()
	root, err := project.CanonicalRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	return root
}
