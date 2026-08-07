package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/luthermonson/shipmates/internal/api"
)

func TestHandlePendingReturnsStructuredJSON(t *testing.T) {
	s := New()
	s.pendings["req-1"] = &pending{
		id: "req-1", persona: "security", tool: "Bash", input: "go test ./...",
	}
	w := httptest.NewRecorder()
	s.handlePending(w, httptest.NewRequest("GET", "/pending", nil))

	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	var got []api.Pending
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "req-1" || got[0].Input != "go test ./..." {
		t.Fatalf("pending = %#v", got)
	}
}
