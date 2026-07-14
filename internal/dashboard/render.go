package dashboard

import (
	"fmt"
	"strings"
)

type Size struct{ Width, Height int }
type Screen struct {
	Lines    []string
	Persona  string
	Planning bool
}

// Render produces a semantic snapshot: no escape sequences, cursor commands,
// credentials, raw identities, or drafts are present in the result.
func Render(m *Model, size Size) Screen {
	if m.Sidebar != nil && size.Width >= 96 {
		return renderWithSidebar(m, size)
	}
	return renderMain(m, size)
}

func renderMain(m *Model, size Size) Screen {
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
	if m.Sidebar != nil {
		legendText = "PgUp/PgDn plan  Home/End  /consult  /sail [--verbose] [--retry-failed]  /quit"
	}
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
	var eventLines []string
	for _, e := range m.Events {
		eventLines = append(eventLines, wrap(e.Text, w)...)
	}
	start := len(eventLines) - room
	if start < 0 {
		start = 0
	}
	for _, line := range eventLines[start:] {
		lines = append(lines, line)
	}
	lines = append(lines, card...)
	if h > 1 {
		lines = append(lines, legend)
	}
	if len(lines) > h {
		lines = lines[:h]
	}
	return Screen{Lines: lines, Persona: m.Persona, Planning: m.Sidebar != nil}
}

func renderWithSidebar(m *Model, size Size) Screen {
	rightWidth := size.Width / 3
	if rightWidth < 30 {
		rightWidth = 30
	}
	leftWidth := size.Width - rightWidth - 3
	left := renderMain(m, Size{Width: leftWidth, Height: size.Height}).Lines
	right := renderSidebar(m.Sidebar, rightWidth, size.Height, m.SidebarScroll)
	lines := make([]string, size.Height)
	for i := range lines {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		lines[i] = pad(l, leftWidth) + " │ " + fit(r, rightWidth)
	}
	return Screen{Lines: lines, Persona: m.Persona, Planning: true}
}

func renderSidebar(sidebar *Sidebar, width, height, offset int) []string {
	if sidebar == nil || height < 1 {
		return nil
	}
	lines := append(wrap(sidebar.Title, width), wrap(sidebar.Status, width)...)
	for _, section := range sidebar.Sections {
		lines = append(lines, wrap(strings.ToUpper(section.Heading), width)...)
		for _, item := range section.Items {
			lines = append(lines, wrap("• "+safeText(item, 512), width)...)
		}
	}
	maxOffset := len(lines) - height
	if maxOffset < 0 {
		maxOffset = 0
	}
	if offset > maxOffset {
		offset = maxOffset
	}
	end := offset + height
	if end > len(lines) {
		end = len(lines)
	}
	return lines[offset:end]
}

func wrap(s string, width int) []string {
	if width < 1 {
		return nil
	}
	runes := []rune(s)
	if len(runes) == 0 {
		return []string{""}
	}
	var lines []string
	for len(runes) > width {
		cut := width
		for i := width; i > 0; i-- {
			if runes[i-1] == ' ' || runes[i-1] == '\t' {
				cut = i - 1
				break
			}
		}
		if cut == 0 {
			cut = width
		}
		lines = append(lines, string(runes[:cut]))
		runes = runes[cut:]
		for len(runes) > 0 && (runes[0] == ' ' || runes[0] == '\t') {
			runes = runes[1:]
		}
	}
	return append(lines, string(runes))
}

func pad(s string, width int) string {
	r := []rune(s)
	if len(r) >= width {
		return fit(s, width)
	}
	return s + strings.Repeat(" ", width-len(r))
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
