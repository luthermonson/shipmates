package commands

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSplitPersona(t *testing.T) {
	raw := []byte("---\nname: lead\ndescription: keeps the ship in order\n---\n\n# Role\n\nBe helpful.\n")
	fm, body := splitPersona(raw)
	if fm.Name != "lead" {
		t.Errorf("Name = %q, want lead", fm.Name)
	}
	if fm.Description != "keeps the ship in order" {
		t.Errorf("Description = %q, want %q", fm.Description, "keeps the ship in order")
	}
	if !strings.HasPrefix(body, "# Role") {
		t.Errorf("body = %q, want it to start with the heading", body)
	}
	if strings.Contains(body, "---") {
		t.Errorf("body still contains frontmatter delimiter: %q", body)
	}
}

func TestSplitPersonaNoFrontmatter(t *testing.T) {
	fm, body := splitPersona([]byte("just a body\nno frontmatter\n"))
	if fm.Name != "" || fm.DomainGlob != nil {
		t.Errorf("frontmatter = %+v, want zero value", fm)
	}
	if body != "just a body\nno frontmatter" {
		t.Errorf("body = %q", body)
	}
}

func TestCondenseBody(t *testing.T) {
	body := strings.Join([]string{
		"# Role",
		"",
		"- At session start, read your memory first.",
		"- Do the actual work.",
		"",
		"## Memory",
		"",
		"This whole section should be dropped.",
		"",
		"## Style",
		"",
		"Keep it terse.",
	}, "\n")

	got := condenseBody(body)

	if strings.Contains(got, "## Memory") {
		t.Error("condenseBody kept the ## Memory heading")
	}
	if strings.Contains(got, "This whole section should be dropped") {
		t.Error("condenseBody kept the Memory section body")
	}
	if strings.Contains(strings.ToLower(got), "read your memory") {
		t.Error("condenseBody kept the 'read your memory' bullet")
	}
	if !strings.Contains(got, "Do the actual work.") {
		t.Error("condenseBody dropped a non-memory bullet")
	}
	if !strings.Contains(got, "## Style") {
		t.Error("condenseBody dropped the ## Style section")
	}
}

func TestParseFrontmatter(t *testing.T) {
	s := strings.Join([]string{
		"name: lead",
		`description: "keeps order"`,
		"byline: 'the team lead'",
		"# a comment line",
		"domainGlob:",
		"  - \"**/*.go\"",
		"  - cmd/**",
	}, "\n")

	fm := parseFrontmatter(s)
	if fm.Name != "lead" {
		t.Errorf("Name = %q, want lead", fm.Name)
	}
	if fm.Description != "keeps order" {
		t.Errorf("Description = %q, want %q", fm.Description, "keeps order")
	}
	if fm.Byline != "the team lead" {
		t.Errorf("Byline = %q, want %q", fm.Byline, "the team lead")
	}
	want := []string{"**/*.go", "cmd/**"}
	if !reflect.DeepEqual(fm.DomainGlob, want) {
		t.Errorf("DomainGlob = %v, want %v", fm.DomainGlob, want)
	}
}

func TestUpsertMarkedSectionIdempotent(t *testing.T) {
	t.Chdir(t.TempDir())
	const path = "AGENTS.md"

	if err := upsertMarkedSection(path, "lead", "lead content v1"); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	if err := upsertMarkedSection(path, "lead", "lead content v2"); err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)

	const startMarker = "<!-- shipmates:lead:start -->"
	if n := strings.Count(text, startMarker); n != 1 {
		t.Fatalf("lead start marker count = %d, want exactly 1\n%s", n, text)
	}
	if strings.Contains(text, "lead content v1") {
		t.Error("old content v1 not replaced")
	}
	if !strings.Contains(text, "lead content v2") {
		t.Error("new content v2 missing")
	}

	if err := upsertMarkedSection(path, "tester", "tester content"); err != nil {
		t.Fatalf("tester upsert: %v", err)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text = string(b)
	if !strings.Contains(text, "lead content v2") {
		t.Error("tester upsert clobbered lead's block")
	}
	if !strings.Contains(text, "tester content") {
		t.Error("tester block not appended")
	}
	if strings.Count(text, startMarker) != 1 {
		t.Error("lead marker duplicated after tester upsert")
	}
}

func TestRenderAgentsMD(t *testing.T) {
	fm := frontmatter{
		Name:        "lead",
		Description: "keeps order",
		DomainGlob:  []string{"**/*.go"},
	}
	out := renderAgentsMD(fm, "# Role\n\nBe helpful.")
	if !strings.Contains(out, "## lead") {
		t.Errorf("renderAgentsMD output missing name heading:\n%s", out)
	}
	if !strings.Contains(out, "keeps order") {
		t.Errorf("renderAgentsMD output missing description:\n%s", out)
	}
	if !strings.Contains(out, "**/*.go") {
		t.Errorf("renderAgentsMD output missing domain glob:\n%s", out)
	}
}
