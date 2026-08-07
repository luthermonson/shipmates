package persona

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	raw := []byte(strings.Join([]string{
		"---",
		"name: captain",
		`description: "keeps order"`,
		"byline: 'the team captain'",
		"domainGlob:",
		`  - "**/*.go"`,
		"  - cmd/**",
		"permissions:",
		"  mode: plan",
		"---",
		"",
		"# Role",
	}, "\n"))

	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frontmatter.Name != "captain" || got.Frontmatter.Description != "keeps order" {
		t.Fatalf("frontmatter = %+v", got.Frontmatter)
	}
	if got.Frontmatter.Byline != "the team captain" {
		t.Fatalf("Byline = %q", got.Frontmatter.Byline)
	}
	if want := []string{"**/*.go", "cmd/**"}; !reflect.DeepEqual(got.Frontmatter.DomainGlob, want) {
		t.Fatalf("DomainGlob = %v, want %v", got.Frontmatter.DomainGlob, want)
	}
	if got.Frontmatter.Permissions.Mode != "plan" {
		t.Fatalf("permission mode = %q", got.Frontmatter.Permissions.Mode)
	}
	if got.Body != "# Role" {
		t.Fatalf("Body = %q", got.Body)
	}
}

func TestParseWithoutFrontmatter(t *testing.T) {
	got, err := Parse([]byte("just a body\nno frontmatter\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Frontmatter.Name != "" || got.Body != "just a body\nno frontmatter" {
		t.Fatalf("definition = %+v", got)
	}
}

func TestParseRequiresExactClosingDelimiter(t *testing.T) {
	raw := []byte("---\nname: captain\n---not-a-delimiter\nbody")
	got, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Frontmatter.Name != "" || !strings.Contains(got.Body, "---not-a-delimiter") {
		t.Fatalf("definition = %+v", got)
	}
}
