package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/luthermonson/shipmates/internal/dashboard"
)

func TestSkipperConsultation(t *testing.T) {
	events := []dashboard.DisplayEvent{
		{Sequence: 7, Kind: "agent", Text: "agent: ordinary planning response"},
		{Sequence: 8, Kind: "agent", Partial: true, Text: "agent: " + skipperConsultPrefix + " incomplete"},
		{Sequence: 9, Kind: "agent", Text: "agent: " + skipperConsultPrefix + " Should Beads remain optional?"},
	}
	question, sequence, index, ok := skipperConsultation(events, 7)
	if !ok {
		t.Fatal("expected automatic consultation request")
	}
	if question != "Should Beads remain optional?" || sequence != 9 || index != 2 {
		t.Fatalf("unexpected consultation: question=%q sequence=%d index=%d", question, sequence, index)
	}
	if _, _, _, ok := skipperConsultation(events, sequence); ok {
		t.Fatal("handled consultation must not be returned again")
	}
}

func TestInvalidVoyageDraft(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voyage.json")
	invalid := `{"version":1,"title":"test","objective":"test","approved":false,"tasks":[{"id":"verify","persona":"tester","summary":"verify","prompt":"verify","models":["luna","terra","sol"],"efforts":["medium","high","xhigh"],"retry_safe":true}]}`
	if err := os.WriteFile(path, []byte(invalid), 0o600); err != nil {
		t.Fatal(err)
	}
	hash, err := invalidVoyageDraft(path)
	if err == nil || !strings.Contains(err.Error(), "exceeds eight tiers") {
		t.Fatalf("expected bounded ladder validation error, got %v", err)
	}
	if hash == ([32]byte{}) {
		t.Fatal("invalid draft must return a stable content hash")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := invalidVoyageDraft(path); err != nil {
		t.Fatalf("missing draft is normal before planning begins: %v", err)
	}
}

func TestSkipperConsultationRejectsInvalidFinalResponse(t *testing.T) {
	tests := []dashboard.DisplayEvent{
		{Sequence: 1, Kind: "agent", Text: "agent: Please run /consult now"},
		{Sequence: 2, Kind: "agent", Text: "agent: " + skipperConsultPrefix},
		{Sequence: 3, Kind: "notice", Text: "agent: " + skipperConsultPrefix + " ignored"},
	}
	for _, event := range tests {
		if _, _, _, ok := skipperConsultation([]dashboard.DisplayEvent{event}, 0); ok {
			t.Fatalf("accepted invalid event: %+v", event)
		}
	}
}

func TestSkipperConsultationBoundsQuestion(t *testing.T) {
	event := dashboard.DisplayEvent{Sequence: 1, Kind: "agent", Text: "agent: " + skipperConsultPrefix + " " + strings.Repeat("x", 4096)}
	question, _, _, ok := skipperConsultation([]dashboard.DisplayEvent{event}, 0)
	if !ok || len(question) > 2048 {
		t.Fatalf("question was not bounded: ok=%v length=%d", ok, len(question))
	}
}
