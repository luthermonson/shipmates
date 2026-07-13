package dashboard

import "fmt"

type Size struct{ Width, Height int }
type Screen struct{ Lines []string }

// Render produces a semantic snapshot: no escape sequences, cursor commands,
// credentials, raw identities, or drafts are present in the result.
func Render(m *Model, size Size) Screen {
	w, h := size.Width, size.Height
	if w < 1 || h < 1 {
		return Screen{}
	}
	status := "observer"
	if m.Controller {
		status = "controller"
	}
	if !m.Connected {
		status += " disconnected"
	}
	lines := []string{fit(fmt.Sprintf("shipmates open %s | %s | %s", safeText(m.Persona, 128), m.State, status), w)}
	if len(m.Images) > 0 {
		var total uint64
		for _, im := range m.Images {
			total += im.Size
		}
		lines = append(lines, fit(fmt.Sprintf("images: %d | %d bytes", len(m.Images), total), w))
		for i, im := range m.Images {
			lines = append(lines, fit(fmt.Sprintf("%d: %s", i+1, safeText(im.name, 128)), w))
		}
	}
	legendText := "/help  /interrupt  /detach  /quit  // literal slash"
	if m.PendingApproval != nil {
		legendText = "/allow-once  /deny  /interrupt  /detach  /quit"
	}
	legend := fit(legendText, w)
	room := h - len(lines) - 1
	card := []string(nil)
	if p := m.PendingApproval; p != nil {
		card = []string{
			fit(fmt.Sprintf("approval pending | %s | policy %s | %dms", safeText(p.RequestKind, 64), safeText(p.PolicyEffect, 16), p.ExpiresInMS), w),
			fit("request: "+safeText(p.Summary, 160), w),
		}
		room -= len(card)
	}
	if room < 0 {
		room = 0
	}
	start := len(m.Events) - room
	if start < 0 {
		start = 0
	}
	for _, e := range m.Events[start:] {
		lines = append(lines, fit(e.Text, w))
	}
	lines = append(lines, card...)
	if h > 1 {
		lines = append(lines, legend)
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return Screen{Lines: lines}
}

func fit(s string, width int) string {
	r := []rune(s)
	if len(r) <= width {
		return s
	}
	if width == 1 {
		return "…"
	}
	return string(r[:width-1]) + "…"
}
