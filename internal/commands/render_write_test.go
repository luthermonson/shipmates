package commands

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/luthermonson/shipmates/internal/catalog"
)

const testPersonaFile = `---
name: geordi
description: keeps the warp core honest
byline: geordi here
domainGlob:
  - "**/*.go"
  - internal/**
---

# Role

Do engineering work.

## Memory

Read everything in ` + "`.shipmates/memory/geordi/`" + ` first.

## Style

Terse.
`

func renderCatalog() *catalog.Catalog {
	return catalog.New(fstest.MapFS{
		"catalog/geordi/.claude/agents/geordi.md": {Data: []byte(testPersonaFile)},
	})
}

// ---------------------------------------------------------------------------
// Render — argument and flag handling
// ---------------------------------------------------------------------------

func TestRenderCommand_ArgAndFlagErrors(t *testing.T) {
	cat := renderCatalog()
	cases := []struct {
		name    string
		args    []string
		wantSub string
	}{
		{"missing persona", []string{"render", "--target", "cursor"}, "usage:"},
		{"unknown persona", []string{"render", "--target", "cursor", "nobody"}, `unknown persona "nobody"`},
		{"unknown target", []string{"render", "--target", "emacs", "geordi"}, `unknown target "emacs"`},
		{"missing required target flag", []string{"render", "geordi"}, "target"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var err error
			_ = captureStdout(t, func() {
				err = Render(cat).Run(context.Background(), tc.args)
			})
			if err == nil {
				t.Fatalf("args %v: expected an error", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("err = %v, want it to mention %q", err, tc.wantSub)
			}
		})
	}
}

// Without --write, render prints to stdout and touches no files.
func TestRenderCommand_StdoutByDefault(t *testing.T) {
	t.Chdir(t.TempDir())
	var err error
	out := captureStdout(t, func() {
		err = Render(renderCatalog()).Run(context.Background(), []string{"render", "--target", "agents-md", "geordi"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## geordi") {
		t.Errorf("nothing rendered to stdout:\n%s", out)
	}
	if _, err := os.Stat("AGENTS.md"); !os.IsNotExist(err) {
		t.Errorf("render without --write created AGENTS.md (err=%v)", err)
	}
}

// --write sends each target to its canonical destination.
func TestRenderCommand_WriteTargets(t *testing.T) {
	cases := []struct {
		target string
		path   string
	}{
		{"cursor", filepath.Join(".cursor", "rules", "geordi.mdc")},
		{"agents-md", "AGENTS.md"},
		{"windsurf", filepath.Join(".windsurf", "rules.md")},
	}
	for _, tc := range cases {
		t.Run(tc.target, func(t *testing.T) {
			t.Chdir(t.TempDir())
			var err error
			_ = captureStdout(t, func() {
				err = Render(renderCatalog()).Run(context.Background(),
					[]string{"render", "--target", tc.target, "--write", "geordi"})
			})
			if err != nil {
				t.Fatalf("render --write: %v", err)
			}
			got := readFile(t, tc.path)
			if !strings.Contains(got, "geordi") {
				t.Errorf("%s missing persona content:\n%s", tc.path, got)
			}
			// Thin targets have no memory loop — the memory section must be
			// stripped on the way out.
			if strings.Contains(got, "## Memory") {
				t.Errorf("%s carries the Memory section:\n%s", tc.path, got)
			}
			if !strings.Contains(got, "Terse.") {
				t.Errorf("%s dropped a non-memory section:\n%s", tc.path, got)
			}
		})
	}
}

// The cursor target is a standalone file: re-rendering replaces it wholesale
// rather than appending.
func TestWriteRender_CursorOverwrites(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join(".cursor", "rules", "geordi.mdc")
	if err := writeRender("cursor", "geordi", "first\n"); err != nil {
		t.Fatal(err)
	}
	if err := writeRender("cursor", "geordi", "second\n"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	if got != "second\n" {
		t.Errorf("cursor rule = %q, want it fully replaced", got)
	}
}

// The append/update targets must preserve everything outside their markers.
func TestWriteRender_PreservesUserContent(t *testing.T) {
	for _, target := range []string{"agents-md", "windsurf"} {
		t.Run(target, func(t *testing.T) {
			t.Chdir(t.TempDir())
			path := "AGENTS.md"
			if target == "windsurf" {
				path = filepath.Join(".windsurf", "rules.md")
			}
			mustWrite(t, path, "# House rules\n\nAlways run the linter.\n")

			if err := writeRender(target, "geordi", "geordi block\n"); err != nil {
				t.Fatal(err)
			}
			got := readFile(t, path)
			if !strings.Contains(got, "Always run the linter.") {
				t.Errorf("pre-existing content lost:\n%s", got)
			}
			if !strings.Contains(got, "geordi block") {
				t.Errorf("persona block not appended:\n%s", got)
			}

			// Re-render must not stack a second block.
			if err := writeRender(target, "geordi", "geordi block v2\n"); err != nil {
				t.Fatal(err)
			}
			got = readFile(t, path)
			if n := strings.Count(got, "<!-- shipmates:geordi:start -->"); n != 1 {
				t.Errorf("marker count = %d, want 1:\n%s", n, got)
			}
			if strings.Contains(got, "geordi block\n<!--") {
				t.Errorf("stale block content survived:\n%s", got)
			}
		})
	}
}

func TestWriteRender_UnknownTarget(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := writeRender("emacs", "geordi", "x"); err == nil {
		t.Error("expected an error for an unknown write target")
	}
}

// upsertMarkedSection normalizes CRLF so a Windows-edited AGENTS.md doesn't
// end up with mixed endings after a render.
func TestUpsertMarkedSection_CRLFInput(t *testing.T) {
	t.Chdir(t.TempDir())
	mustWrite(t, "AGENTS.md", "# Rules\r\n\r\nline two\r\n")
	if err := upsertMarkedSection("AGENTS.md", "geordi", "block"); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, "AGENTS.md")
	if strings.Contains(got, "\r") {
		t.Errorf("CRLF survived into the rewritten file: %q", got)
	}
	if !strings.Contains(got, "line two") {
		t.Errorf("existing content lost:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// parseFlowList / parseFrontmatter edge cases
// ---------------------------------------------------------------------------

func TestParseFlowList(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{`["a", "b"]`, []string{"a", "b"}},
		{`[a,b]`, []string{"a", "b"}},
		{`['a']`, []string{"a"}},
		{`[ a , b , ]`, []string{"a", "b"}}, // trailing comma yields no empty entry
		{`[]`, nil},
		{``, nil},
		{`a, b`, []string{"a", "b"}}, // brackets are optional
	}
	for _, tc := range cases {
		got := parseFlowList(tc.in)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("parseFlowList(%q) = %#v, want %#v", tc.in, got, tc.want)
		}
	}
}

func TestParseFrontmatter_FlowStyleDomainGlob(t *testing.T) {
	fm := parseFrontmatter("name: geordi\ndomainGlob: [\"**/*.go\", \"cmd/**\"]\n")
	want := []string{"**/*.go", "cmd/**"}
	if !reflect.DeepEqual(fm.DomainGlob, want) {
		t.Errorf("DomainGlob = %#v, want %#v", fm.DomainGlob, want)
	}
}

// Nested keys (indented) belong to some other structure and must not be
// mistaken for top-level frontmatter fields.
func TestParseFrontmatter_IgnoresIndentedKeys(t *testing.T) {
	fm := parseFrontmatter("name: geordi\npermissions:\n  description: not the persona description\n")
	if fm.Name != "geordi" {
		t.Errorf("Name = %q", fm.Name)
	}
	if fm.Description != "" {
		t.Errorf("Description picked up a nested key: %q", fm.Description)
	}
}

// A block-sequence domainGlob followed by another key must not swallow it.
func TestParseFrontmatter_KeyAfterBlockSequence(t *testing.T) {
	s := "domainGlob:\n  - \"**/*.go\"\n  - cmd/**\nbyline: geordi here\n"
	fm := parseFrontmatter(s)
	if len(fm.DomainGlob) != 2 {
		t.Errorf("DomainGlob = %#v, want 2 entries", fm.DomainGlob)
	}
	if fm.Byline != "geordi here" {
		t.Errorf("byline after a block sequence was lost: %q", fm.Byline)
	}
}

func TestUnquoteScalar(t *testing.T) {
	cases := map[string]string{
		`"quoted"`:    "quoted",
		`'quoted'`:    "quoted",
		`bare`:        "bare",
		`"unclosed`:   `"unclosed`,
		`''`:          "",
		`"`:           `"`, // a single char can't be a quoted pair
		`he said "x"`: `he said "x"`,
	}
	for in, want := range cases {
		if got := unquoteScalar(in); got != want {
			t.Errorf("unquoteScalar(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// splitPersona edge cases
// ---------------------------------------------------------------------------

func TestSplitPersona_EdgeCases(t *testing.T) {
	t.Run("unterminated frontmatter is treated as body", func(t *testing.T) {
		fm, body := splitPersona([]byte("---\nname: geordi\nno closing delimiter\n"))
		if fm.Name != "" {
			t.Errorf("parsed frontmatter from an unterminated block: %+v", fm)
		}
		if !strings.Contains(body, "name: geordi") {
			t.Errorf("body = %q, want the raw text", body)
		}
	})

	t.Run("CRLF persona file parses", func(t *testing.T) {
		fm, body := splitPersona([]byte("---\r\nname: geordi\r\n---\r\n\r\n# Role\r\n"))
		if fm.Name != "geordi" {
			t.Errorf("Name = %q, want geordi", fm.Name)
		}
		if !strings.HasPrefix(body, "# Role") {
			t.Errorf("body = %q", body)
		}
	})

	t.Run("leading blank lines before the frontmatter", func(t *testing.T) {
		fm, _ := splitPersona([]byte("\n\n---\nname: geordi\n---\n\nbody\n"))
		if fm.Name != "geordi" {
			t.Errorf("Name = %q, want geordi", fm.Name)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		fm, body := splitPersona(nil)
		if fm.Name != "" || body != "" {
			t.Errorf("got (%+v, %q), want zero values", fm, body)
		}
	})
}

// ---------------------------------------------------------------------------
// renderCursor / renderWindsurf shape
// ---------------------------------------------------------------------------

func TestRenderCursor(t *testing.T) {
	fm := frontmatter{Name: "geordi", Description: "keeps order", DomainGlob: []string{"**/*.go", "cmd/**"}}
	out := renderCursor(fm, "# Role\n\nWork.")

	if !strings.HasPrefix(out, "---\n") {
		t.Errorf("cursor rule must open with frontmatter:\n%s", out)
	}
	if !strings.Contains(out, "description: keeps order") {
		t.Errorf("description missing:\n%s", out)
	}
	// Cursor wants a comma-separated glob list with no spaces.
	if !strings.Contains(out, "globs: **/*.go,cmd/**") {
		t.Errorf("globs not in cursor's comma-separated form:\n%s", out)
	}
	if !strings.Contains(out, "alwaysApply: false") {
		t.Errorf("alwaysApply missing:\n%s", out)
	}
	if !strings.Contains(out, "# geordi") {
		t.Errorf("persona heading missing:\n%s", out)
	}
}

func TestRenderCursor_NoOptionalFields(t *testing.T) {
	out := renderCursor(frontmatter{Name: "geordi"}, "body")
	if strings.Contains(out, "description:") {
		t.Errorf("emitted an empty description key:\n%s", out)
	}
	if strings.Contains(out, "globs:") {
		t.Errorf("emitted an empty globs key:\n%s", out)
	}
	if !strings.Contains(out, "alwaysApply: false") {
		t.Errorf("alwaysApply missing:\n%s", out)
	}
}

func TestRenderWindsurf(t *testing.T) {
	fm := frontmatter{Name: "geordi", Description: "keeps order", DomainGlob: []string{"**/*.go"}}
	out := renderWindsurf(fm, "# Role\n\nWork.")
	for _, want := range []string{"## geordi", "keeps order", "_Globs: **/*.go_"} {
		if !strings.Contains(out, want) {
			t.Errorf("windsurf output missing %q:\n%s", want, out)
		}
	}
}

// A persona whose frontmatter omits `name` falls back to the CLI argument, so
// thin targets never render a nameless heading.
func TestRenderCommand_NameFallsBackToArgument(t *testing.T) {
	t.Chdir(t.TempDir())
	cat := catalog.New(fstest.MapFS{
		"catalog/nameless/.claude/agents/nameless.md": {Data: []byte("---\ndescription: x\n---\n\n# Body\n")},
	})
	var err error
	out := captureStdout(t, func() {
		err = Render(cat).Run(context.Background(), []string{"render", "--target", "agents-md", "nameless"})
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "## nameless") {
		t.Errorf("name did not fall back to the persona argument:\n%s", out)
	}
}

// condenseBody must not eat a heading merely because it starts with "memor".
func TestCondenseBody_MemorableHeadingKept(t *testing.T) {
	body := "## Memorable moments\n\nkeep this\n\n## Memory\n\ndrop this\n"
	got := condenseBody(body)
	if !strings.Contains(got, "Memorable moments") || !strings.Contains(got, "keep this") {
		t.Errorf("a non-memory heading was dropped:\n%s", got)
	}
	if strings.Contains(got, "drop this") {
		t.Errorf("the Memory section survived:\n%s", got)
	}
}
