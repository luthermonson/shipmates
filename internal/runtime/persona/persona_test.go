package persona

import (
	"strings"
	"testing"
)

func TestParse_FullFrontmatter(t *testing.T) {
	in := `---
name: architect
description: Cross-cutting design review.
byline: "Architect here, —"
domainGlob:
  - "**/*.md"
  - "docs/**/*"
memoryDir: .shipmates/memory/architect
permissions:
  mode: acceptEdits
remoteControl: false
---

# Role

Body content follows.
`
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Name != "architect" {
		t.Errorf("name=%q", c.Name)
	}
	if len(c.DomainGlob) != 2 {
		t.Errorf("domainGlob len=%d", len(c.DomainGlob))
	}
	if c.MemoryDir != ".shipmates/memory/architect" {
		t.Errorf("memoryDir=%q", c.MemoryDir)
	}
	if !strings.Contains(c.Body, "# Role") {
		t.Errorf("body missing Role section: %q", c.Body)
	}
}

func TestParse_MissingName(t *testing.T) {
	in := `---
description: no name
---

body
`
	_, err := Parse(strings.NewReader(in))
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestParse_NoFrontmatter(t *testing.T) {
	_, err := Parse(strings.NewReader("just body\nno frontmatter\n"))
	if err == nil {
		t.Fatal("expected error when no frontmatter and thus no name")
	}
}

// TestParse_NormalizesCRLF pins determinism across checkouts. The catalog is
// embedded from the working tree, so on Windows it arrives with CRLF; a
// runtime artifact rendered from it must be byte-identical to the one
// rendered on Linux, because the install manifest hashes those bytes and a
// stray \r would make every `update` look like a local edit.
func TestParse_NormalizesCRLF(t *testing.T) {
	lf := "---\nname: backend\ndescription: Backend review.\n---\n\n# Body\nline two\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	a, err := Parse(strings.NewReader(lf))
	if err != nil {
		t.Fatal(err)
	}
	b, err := Parse(strings.NewReader(crlf))
	if err != nil {
		t.Fatalf("parse CRLF: %v", err)
	}
	if b.Body != a.Body {
		t.Errorf("CRLF body = %q, want %q", b.Body, a.Body)
	}
	if strings.Contains(b.Body, "\r") {
		t.Errorf("carriage return survived: %q", b.Body)
	}
	if b.Name != a.Name || b.Description != a.Description {
		t.Errorf("CRLF frontmatter = %+v, want %+v", b, a)
	}
}

func TestWrite_RoundTrip(t *testing.T) {
	in := `---
name: backend
description: Backend review.
memoryDir: .shipmates/memory/backend
---

# Body
line two
`
	c, err := Parse(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if err := c.Write(&out); err != nil {
		t.Fatal(err)
	}
	c2, err := Parse(strings.NewReader(out.String()))
	if err != nil {
		t.Fatalf("reparse: %v\n%s", err, out.String())
	}
	if c2.Name != c.Name || c2.MemoryDir != c.MemoryDir || c2.Body != c.Body {
		t.Errorf("round-trip mismatch:\nfirst: %+v\nsecond: %+v", c, c2)
	}
}
