package commands

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/project"
)

func TestLegacyCaptainMigrationArchivesEditedAgentAndReservesHumanRole(t *testing.T) {
	t.Chdir(t.TempDir())
	files := fstest.MapFS{}
	for _, name := range []string{"captain", "quartermaster", "skipper"} {
		files["catalog/"+name+"/agent.md"] = &fstest.MapFile{Data: []byte("---\nname: " + name + "\ndescription: test\n---\n\nRole for " + name + ".\n")}
		files["catalog/"+name+"/policy.yaml"] = &fstest.MapFile{Data: []byte(emptyStrictPolicy)}
	}
	cat := catalog.New(files)
	if err := Init(cat).Run(context.Background(), []string{"shipmates", "--crew", "captain"}); err != nil {
		t.Fatal(err)
	}
	config, err := project.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(config.ModelLadder, ","); got != "gpt-5.6-luna,gpt-5.6-terra,gpt-5.6-sol" {
		t.Fatalf("generated model ladder = %q", got)
	}
	captainPath := project.CodexAgentPath("captain")
	f, err := os.OpenFile(captainPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("# edited by user\n")
	_ = f.Close()
	if err := os.MkdirAll(project.MemoryDir("captain"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project.MemoryDir("captain"), "history.md"), []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runUpdate(cat, "", "ours"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"quartermaster", "skipper"} {
		if _, err := os.Stat(project.CodexAgentPath(name)); err != nil {
			t.Fatalf("%s not installed: %v", name, err)
		}
	}
	if _, err := os.Stat(captainPath); !os.IsNotExist(err) {
		t.Fatalf("captain remains executable: %v", err)
	}
	archived, err := os.ReadFile(filepath.Join(project.Dir, "legacy", "captain", "agent.toml"))
	if err != nil || !strings.Contains(string(archived), "edited by user") {
		t.Fatalf("edited captain archive = %q, %v", archived, err)
	}
	if b, err := os.ReadFile(filepath.Join(project.MemoryDir("captain"), "history.md")); err != nil || string(b) != "keep me" {
		t.Fatalf("captain memory = %q, %v", b, err)
	}
	if err := dispatch(context.Background(), "captain", "test", false); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("captain dispatch error = %v", err)
	}
}
