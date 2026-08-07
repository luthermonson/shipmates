package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestEventHistoryBoundedAndCursorBased(t *testing.T) {
	s := New()
	for i := 0; i < maxEventHistory+10; i++ {
		s.addEvent(Event{Persona: "tester", Type: "test", Text: fmt.Sprintf("event-%d", i)})
	}

	req := httptest.NewRequest(http.MethodGet, "/events", nil)
	rec := httptest.NewRecorder()
	s.handleEventsJSON(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200", rec.Code)
	}
	var events []Event
	if err := json.NewDecoder(rec.Body).Decode(&events); err != nil {
		t.Fatal(err)
	}
	if len(events) != maxEventHistory {
		t.Fatalf("retained events = %d, want %d", len(events), maxEventHistory)
	}
	for i, e := range events {
		if e.Seq == 0 {
			t.Fatalf("event %d has zero sequence", i)
		}
		if i > 0 && e.Seq <= events[i-1].Seq {
			t.Fatalf("sequence not increasing at %d: %d <= %d", i, e.Seq, events[i-1].Seq)
		}
	}

	after := events[len(events)-3].Seq
	req = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/events?after=%d", after), nil)
	rec = httptest.NewRecorder()
	s.handleEventsJSON(rec, req)
	var tail []Event
	if err := json.NewDecoder(rec.Body).Decode(&tail); err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 {
		t.Fatalf("cursor tail = %d events, want 2", len(tail))
	}
	if tail[0].Seq <= after {
		t.Fatalf("first tail sequence = %d, want > %d", tail[0].Seq, after)
	}
}

func TestEventCursorRejectsInvalidValue(t *testing.T) {
	s := New()
	req := httptest.NewRequest(http.MethodGet, "/events?after=not-a-number", nil)
	rec := httptest.NewRecorder()
	s.handleEventsJSON(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
