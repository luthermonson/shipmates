package brig

import "strings"

// PromptStartMarker and PromptEndMarker delimit the Articles reminder block
// inside a persona artifact. Exported so every runtime installer can splice
// and replace the block idempotently, and so `shipmates update` recomposition
// never stacks a second copy.
const (
	PromptStartMarker = "<!-- shipmates:articles:start -->"
	PromptEndMarker   = "<!-- shipmates:articles:end -->"
)

// PromptBlock renders the Ship's Articles reminder for a persona artifact,
// honoring the operator's Settings: waived Articles are left out, and a
// disabled brig renders nothing at all.
//
// The block is a short reminder pointing personas at the full canonical
// rules and the Brig's enforcement layers — not the entire Articles text
// (that would balloon every persona's context). Kernel and freeze
// enforcement bind regardless of whether this block is present; the block
// makes the binding legible from inside the session.
func PromptBlock(s Settings) string {
	if !s.Enabled {
		return ""
	}
	var code, conduct []Rule
	for _, r := range canonicalRules {
		if s.Disabled(r.Handle) {
			continue
		}
		if r.Category == CategoryCode {
			code = append(code, r)
		} else {
			conduct = append(conduct, r)
		}
	}
	if len(code) == 0 && len(conduct) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(PromptStartMarker)
	b.WriteString("\n## The Ship's Articles\n\n")
	b.WriteString("You are bound by the Ship's Articles — see `.shipmates/ARTICLES.md` (or\n")
	b.WriteString("`catalog/ARTICLES.md` in the shipmates repo) for the full text.\n")
	if len(code) > 0 {
		b.WriteString("\n**Articles of Code** — follow in every change you make: ")
		b.WriteString(joinTitles(code))
		b.WriteString(".\n")
	}
	if len(conduct) > 0 {
		b.WriteString("\n**Articles of Conduct** — kernel-enforced; violating one sends you to the Brig: ")
		b.WriteString(joinTitles(conduct))
		b.WriteString(".\n")
	}
	if !s.Disabled("respect-the-freeze") {
		b.WriteString("\nIf `.shipmates/freeze` exists, refuse all Write and Edit operations until an\nadmiral clears it with `shipmates release`.\n")
	}
	b.WriteString("If unsure whether an action is Article-safe, stop and ask the operator.\n")
	b.WriteString(PromptEndMarker)
	return b.String()
}

func joinTitles(rules []Rule) string {
	titles := make([]string, 0, len(rules))
	for _, r := range rules {
		titles = append(titles, r.Title)
	}
	return strings.Join(titles, "; ")
}

// SplicePrompt returns body with the given Articles block appended in its
// marker-delimited section. Idempotent: an existing block is replaced in
// place, so re-installing or updating a persona never stacks copies. An
// empty block (brig disabled) removes any existing block instead.
func SplicePrompt(body, block string) string {
	start := strings.Index(body, PromptStartMarker)
	end := strings.Index(body, PromptEndMarker)
	if start == -1 || end == -1 || end < start {
		// No existing block.
		if block == "" {
			return body
		}
		trimmed := strings.TrimRight(body, "\n")
		if strings.TrimSpace(trimmed) == "" {
			return block + "\n"
		}
		return trimmed + "\n\n" + block + "\n"
	}
	// Replace (or remove) the existing block. Consume the end marker's
	// trailing newline so re-runs are byte-stable.
	tail := end + len(PromptEndMarker)
	if tail < len(body) && body[tail] == '\n' {
		tail++
	}
	before := strings.TrimRight(body[:start], " \t\n")
	after := strings.TrimLeft(body[tail:], "\n")
	var parts []string
	if before != "" {
		parts = append(parts, before)
	}
	if block != "" {
		parts = append(parts, block)
	}
	if after != "" {
		parts = append(parts, strings.TrimRight(after, "\n"))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n\n") + "\n"
}
