package project

import (
	"strings"
	"testing"
)

func TestM10CodexTOMLStrictManagedField(t *testing.T) {
	bad := []string{
		"developer_instructions = \"one\"\ndeveloper_instructions = \"two\"\n",
		"[agent]\ndeveloper_instructions = \"nested\"\n",
		"developer_instructions = 42\n",
		"developer_instructions = \"ok\"\n[x]\na=1\n[x]\nb=2\n",
	}
	for _, raw := range bad {
		if _, err := ParseCodexAgent([]byte(raw)); err == nil {
			t.Errorf("accepted %q", raw)
		}
	}
}

func TestM10CodexTOMLReplacementPreservesUnrelatedFieldsAndComments(t *testing.T) {
	raw := []byte("# leading comment\nname = \"tester\" # identity\ndeveloper_instructions = \"old\" # managed\nmodel = \"gpt-test\"\n[metadata]\ncolor = \"blue\" # keep me\n")
	doc, err := ParseCodexAgent(raw)
	if err != nil {
		t.Fatal(err)
	}
	out, err := doc.ReplaceInstructions("new routing instructions")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"leading comment", "identity", "managed", "gpt-test", "keep me", "color = \"blue\""} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lost %q:\n%s", want, out)
		}
	}
	parsed, err := ParseCodexAgent(out)
	if err != nil || parsed.Instructions != "new routing instructions" {
		t.Fatalf("replacement = %#v, %v", parsed, err)
	}
}
