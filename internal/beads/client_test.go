//go:build !windows

package beads

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	actual := t.TempDir()
	if err := os.Symlink(actual, filepath.Join(root, ".beads")); err != nil {
		t.Fatal(err)
	}
	if Workspace(root) {
		t.Fatal("symlinked .beads directory accepted")
	}
}

func TestIssueIDAcceptsObjectAndSingleItemList(t *testing.T) {
	for _, raw := range []string{`{"id":"ship-abc.1"}`, `[{"id":"ship-abc.1"}]`} {
		id, err := issueID(raw)
		if err != nil || id != "ship-abc.1" {
			t.Fatalf("issueID(%q) = %q, %v", raw, id, err)
		}
	}
	for _, raw := range []string{`{"id":"--bad"}`, `{"id":"bad id"}`, `[]`} {
		if _, err := issueID(raw); err == nil {
			t.Fatalf("issueID(%q) succeeded", raw)
		}
	}
}

func TestClientLifecycleUsesBoundedArgvCommands(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "calls.log")
	script := filepath.Join(root, "bd")
	body := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\ncase \"$1\" in\ncreate) printf '%s\\n' '{\"id\":\"ship-abc\"}' ;;\nprime) printf '%s\\n' 'prime context' ;;\nshow) printf '%s\\n' '{\"id\":\"ship-abc\"}' ;;\nesac\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	client := &Client{Root: root, Command: script, Timeout: time.Second}
	id, err := client.CreateTask(context.Background(), Task{
		Title: "bounded task", Description: "do it", Assignee: "backend",
		ExternalRef: "shipmates:voyage:hash:task", Labels: []string{"shipmates", "voyage"},
	})
	if err != nil || id != "ship-abc" {
		t.Fatalf("CreateTask = %q, %v", id, err)
	}
	if err := client.AddDependency(context.Background(), id, "ship-parent"); err != nil {
		t.Fatal(err)
	}
	if err := client.Start(context.Background(), id, "backend"); err != nil {
		t.Fatal(err)
	}
	if err := client.Complete(context.Background(), id, "done"); err != nil {
		t.Fatal(err)
	}
	if got := client.Prime(context.Background()); got != "prime context" {
		t.Fatalf("Prime = %q", got)
	}
	if got := client.Show(context.Background(), id); !strings.Contains(got, id) {
		t.Fatalf("Show = %q", got)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	for _, want := range []string{
		"create --json --type=task --title=bounded task",
		"dep add ship-abc ship-parent",
		"update ship-abc --status=in_progress --assignee=backend",
		"comments add ship-abc done --author=shipmates",
		"close ship-abc --reason=Shipmates voyage task completed",
	} {
		if !strings.Contains(log, want) {
			t.Fatalf("call log missing %q:\n%s", want, log)
		}
	}
}

func TestClientTimeout(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "bd")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	client := &Client{Root: root, Command: script, Timeout: 20 * time.Millisecond}
	if _, err := client.Run(context.Background(), "list"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestClientDoesNotForwardCredentialEnvironment(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "env")
	script := filepath.Join(root, "bd")
	body := "#!/bin/sh\nenv > " + path + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GITHUB_TOKEN", "must-not-reach-bd")
	client := &Client{Root: root, Command: script, Timeout: time.Second}
	if _, err := client.Run(context.Background(), "prime"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "GITHUB_TOKEN") || strings.Contains(string(raw), "must-not-reach-bd") {
		t.Fatalf("credential environment forwarded to bd: %s", raw)
	}
}

func TestClientRejectsUntrustedIssueIDs(t *testing.T) {
	client := &Client{}
	if err := client.AddDependency(context.Background(), "--help", "ship-parent"); err == nil {
		t.Fatal("flag-like issue id accepted")
	}
}
