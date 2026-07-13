package dashboard

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/luthermonson/shipmates/internal/livesession"
	"github.com/luthermonson/shipmates/internal/turninput"
)

const maxDisplayBytes = 16 << 10

var ErrInvalidStream = errors.New("invalid_event_stream")

type DisplayEvent struct {
	Sequence uint64
	Text     string
	Dropped  bool
}

// Model is terminal-independent dashboard state. It retains bounded, already
// redacted presentation strings rather than protocol event payloads.
type Model struct {
	Persona, SessionID string
	State              livesession.State
	Controller         bool
	Connected          bool
	Events             []DisplayEvent
	PendingApproval    *livesession.ApprovalCard
	next               uint64
	Images             []PendingImage
}
type PendingImage struct {
	path, name string
	Size       uint64
}

func (m *Model) AddImage(path string) error {
	paths := m.ImagePaths()
	paths = append(paths, path)
	b, e := turninput.ValidateImages(".", paths)
	if e != nil {
		return e
	}
	defer b.Close()
	m.Images = nil
	for _, d := range b.Images() {
		m.Images = append(m.Images, PendingImage{d.AbsolutePath(), filepath.Base(d.DisplayPath()), d.Size})
	}
	return nil
}
func (m *Model) RemoveImage(index int) error {
	if index < 1 || index > len(m.Images) {
		return errors.New("image_index_invalid")
	}
	m.Images = append(m.Images[:index-1], m.Images[index:]...)
	return nil
}
func (m *Model) ClearImages() { m.Images = nil }
func (m *Model) ImagePaths() []string {
	out := make([]string, len(m.Images))
	for i := range m.Images {
		out[i] = m.Images[i].path
	}
	return out
}

func (m *Model) ApplySnapshot(s livesession.Snapshot) error {
	if s.Persona != m.Persona || s.SessionID != m.SessionID || !validState(s.State) {
		return ErrInvalidStream
	}
	m.State = s.State
	return nil
}

func (m *Model) Notice(text string) {
	m.Events = append(m.Events, DisplayEvent{Text: safeText(text, maxDisplayBytes)})
	if len(m.Events) > 256 {
		m.Events = append([]DisplayEvent(nil), m.Events[len(m.Events)-256:]...)
	}
}

func NewModel(a livesession.Attach) (*Model, error) {
	m := &Model{Controller: a.ControllerID != "", Connected: true}
	if err := m.Reconnect(a); err != nil {
		return nil, err
	}
	return m, nil
}

// Reconnect replaces the lease snapshot. History from a different session is
// discarded; exact duplicates in the same session remain suppressed.
func (m *Model) Reconnect(a livesession.Attach) error {
	if a.Persona == "" || a.SessionID == "" || !validState(a.State) || a.NextSequence < a.OldestAvailable {
		return ErrInvalidStream
	}
	if m.SessionID != a.SessionID {
		m.ClearImages()
		m.Events = nil
		// A fresh session snapshot has no locally expected cursor. Its first
		// retained envelope is the unambiguous baseline chosen by attach.
		m.next = a.NextSequence
		if len(a.Events) > 0 {
			m.next = a.Events[0].Sequence
		} else if a.HistoryDropped {
			m.next = a.OldestAvailable
		}
	}
	m.Persona, m.SessionID, m.State = a.Persona, a.SessionID, a.State
	m.Controller, m.Connected = a.ControllerID != "", true
	m.PendingApproval = a.PendingApproval
	if a.State != livesession.Idle || a.PendingApproval != nil {
		m.ClearImages()
	}
	return m.apply(a.OldestAvailable, a.NextSequence, a.HistoryDropped, a.Events)
}

func (m *Model) ApplyFeed(f livesession.Feed) error {
	return m.apply(f.OldestAvailable, f.NextSequence, f.HistoryDropped, f.Events)
}

func (m *Model) apply(oldest, next uint64, dropped bool, events []livesession.Event) error {
	if next < oldest || m.SessionID == "" {
		return ErrInvalidStream
	}
	if dropped || m.next < oldest {
		m.Events = append(m.Events, DisplayEvent{Text: fmt.Sprintf("history dropped before sequence %d", oldest), Dropped: true})
		m.next = oldest
	}
	for _, e := range events {
		if e.SchemaVersion != 1 || e.Persona != m.Persona || e.SessionID != m.SessionID || e.Sequence < m.next || e.Sequence >= next {
			if e.Sequence < m.next {
				continue
			} // exact sequence duplicates only
			return ErrInvalidStream
		}
		if e.Sequence != m.next {
			return ErrInvalidStream
		}
		if text, ok := presentEvent(e); ok {
			m.Events = append(m.Events, DisplayEvent{Sequence: e.Sequence, Text: text})
		}
		m.next++
	}
	if m.next > next {
		return ErrInvalidStream
	}
	// A response may contain no events because unknown kinds were omitted, but
	// only received envelopes advance the cursor.
	if len(events) > 0 && m.next != next {
		return ErrInvalidStream
	}
	if len(m.Events) > 256 {
		m.Events = append([]DisplayEvent(nil), m.Events[len(m.Events)-256:]...)
	}
	return nil
}

func validState(s livesession.State) bool {
	switch s {
	case livesession.Starting, livesession.Idle, livesession.Working, livesession.Steering, livesession.Interrupting, livesession.Stopped, livesession.Failed:
		return true
	}
	return false
}

func presentEvent(e livesession.Event) (string, bool) {
	switch e.Kind {
	case "agent.message":
		v, ok := e.Data["text"].(string)
		if !ok {
			return "agent message unavailable", true
		}
		v = safeText(v, maxDisplayBytes)
		if trunc, _ := e.Data["truncated"].(bool); trunc {
			v += " [truncated]"
		}
		return "agent: " + v, true
	case "activity":
		v, _ := e.Data["category"].(string)
		switch v {
		case "analysis", "tool", "command", "search", "file", "other":
			return "activity: " + v, true
		}
		return "activity", true
	case "session.starting":
		return "session starting", true
	case "session.ready":
		return "session ready", true
	case "turn.started":
		return "turn started", true
	case "turn.completed":
		return "turn completed", true
	case "turn.interrupted":
		return "turn interrupted", true
	case "turn.failed":
		return "turn failed", true
	case "steering.accepted":
		return "steer accepted", true
	case "steering.refused":
		return "steer refused", true
	case "interrupt.accepted":
		return "interrupt accepted", true
	case "interrupt.refused":
		return "interrupt refused", true
	case "request.refused":
		return "request refused", true
	default:
		return "unsupported event", true
	}
}

func safeText(s string, limit int) string {
	s = strings.ToValidUTF8(s, "�")
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return '�'
		}
		return r
	}, s)
	if len(s) <= limit {
		return s
	}
	for limit > 0 && !utf8.RuneStart(s[limit]) {
		limit--
	}
	return s[:limit] + "…"
}
