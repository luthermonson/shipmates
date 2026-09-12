package beads

import (
	"context"
	"strings"
	"testing"
)

// Fixtures captured verbatim from bd 1.1.2 (see internal/beads/bdtest.Version).
// The real CLI emits a JSON object from `bd create --json` and a JSON array from
// `bd show <id> --json`; issueID must cope with both because a future bd could
// unify them. Keeping the shapes here documents the contract even on machines
// where the real bd is not installed.
const (
	realCreateJSON = `{
  "assignee": "backend",
  "created_at": "2026-07-30T00:05:04.8978543Z",
  "created_by": "Shipmates Test",
  "description": "do it",
  "external_ref": "shipmates:voyage:hash:task",
  "id": "ship-8he",
  "issue_type": "task",
  "labels": ["shipmates", "voyage"],
  "priority": 2,
  "schema_version": 1,
  "status": "open",
  "title": "bounded task",
  "updated_at": "2026-07-30T00:05:04.8978543Z"
}`
	realShowJSON = `[
  {
    "id": "ship-8he",
    "title": "bounded task",
    "status": "open",
    "priority": 2,
    "issue_type": "task",
    "dependent_count": 0,
    "dependency_count": 0,
    "comment_count": 0
  }
]`
)

func TestIssueIDParsesRealBDOutputShapes(t *testing.T) {
	for name, raw := range map[string]string{"create object": realCreateJSON, "show array": realShowJSON} {
		id, err := issueID(raw)
		if err != nil || id != "ship-8he" {
			t.Errorf("issueID(%s) = %q, %v", name, id, err)
		}
	}
}

func TestIssueIDAcceptsObjectAndSingleItemList(t *testing.T) {
	for _, raw := range []string{`{"id":"ship-abc.1"}`, `[{"id":"ship-abc.1"}]`} {
		id, err := issueID(raw)
		if err != nil || id != "ship-abc.1" {
			t.Fatalf("issueID(%q) = %q, %v", raw, id, err)
		}
	}
	for _, raw := range []string{`{"id":"--bad"}`, `{"id":"bad id"}`, `[]`, ``, `not json`, `[{"id":"a"},{"id":"b"}]`} {
		if _, err := issueID(raw); err == nil {
			t.Fatalf("issueID(%q) succeeded", raw)
		}
	}
}

// TestIssueIDRejectsParentDirectoryHop pins the gap that the old local validID
// left open: a bd-sourced id carrying ".." was accepted because that copy had
// the alphabet but not the ..-hop check that internal/beadid enforces. The id
// returned here becomes an argv/path segment downstream, so the hop must be
// refused at the parse boundary.
func TestIssueIDRejectsParentDirectoryHop(t *testing.T) {
	for _, raw := range []string{`{"id":"ship-..evil"}`, `{"id":"a..b"}`, `[{"id":"proj-c03.."}]`} {
		if id, err := issueID(raw); err == nil {
			t.Fatalf("issueID(%q) = %q, want rejection of the .. hop", raw, id)
		}
	}
}

func TestClientRejectsUntrustedIssueIDs(t *testing.T) {
	client := &Client{}
	if err := client.AddDependency(context.Background(), "--help", "ship-parent"); err == nil {
		t.Fatal("flag-like issue id accepted")
	}
	if err := client.AddDependency(context.Background(), "ship-child", "--help"); err == nil {
		t.Fatal("flag-like dependency id accepted")
	}
}

func TestRunRejectsUnconfiguredClient(t *testing.T) {
	for name, client := range map[string]*Client{
		"nil":         nil,
		"no command":  {Root: "."},
		"no root":     {Command: "bd"},
		"zero client": {},
	} {
		if _, err := client.Run(context.Background(), "prime"); err == nil || !strings.Contains(err.Error(), "invalid Beads client") {
			t.Errorf("%s client Run error = %v", name, err)
		}
	}
}

func TestBoundedTruncatesAndTrims(t *testing.T) {
	if got := bounded("  spaced  ", 32); got != "spaced" {
		t.Errorf("bounded = %q", got)
	}
	if got := bounded(strings.Repeat("x", 100), 10); len(got) != 10 {
		t.Errorf("bounded length = %d", len(got))
	}
}

func TestLimitedBufferFlagsOverflow(t *testing.T) {
	buffer := &limitedBuffer{limit: 4}
	if _, err := buffer.Write([]byte("abcdef")); err != nil {
		t.Fatal(err)
	}
	if buffer.String() != "abcd" || !buffer.overflow {
		t.Fatalf("buffer = %q overflow=%v", buffer.String(), buffer.overflow)
	}
}
