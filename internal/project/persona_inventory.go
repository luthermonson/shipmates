package project

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/luthermonson/shipmates/internal/brig"
)

const (
	MaxInstalledPersonas = 256
	MaxCodexAgentBytes   = 256 << 10
)

type InstalledPersona struct {
	Name   string
	Raw    []byte
	Doc    *CodexAgentDocument
	Mode   os.FileMode
	Device uint64
	Inode  uint64
}

// CanonicalPersonaInventory captures the complete canonical inventory through
// platform-specific no-follow handles. Callers receive the exact validated
// bytes, avoiding a validate-by-name/reopen-by-name race.
func CanonicalPersonaInventory(root string) ([]InstalledPersona, error) {
	return readAndValidateCanonicalPersonas(root, "")
}

// CanonicalPersonaAt reads one named canonical artifact through the same
// bounded, no-follow directory-handle walk used by the complete inventory.
// It never validates a pathname and then reopens that pathname by name.
func CanonicalPersonaAt(root, name string) (*InstalledPersona, error) {
	if err := ValidatePersonaName(name); err != nil {
		return nil, err
	}
	items, err := readAndValidateCanonicalPersonas(root, name)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("persona %q is not installed — run: shipmates add %s", name, name)
	}
	return &items[0], nil
}

func readAndValidateCanonicalPersonas(root, name string) ([]InstalledPersona, error) {
	canonical, err := CanonicalRoot(root)
	if err != nil {
		return nil, fmt.Errorf("canonical persona inventory root: %w", err)
	}
	items, err := readCanonicalPersonaInventory(canonical, name)
	if err != nil {
		return nil, err
	}
	for i := range items {
		if err := ValidatePersonaName(items[i].Name); err != nil {
			return nil, fmt.Errorf("unsafe entry in Codex persona inventory: %w", err)
		}
		doc, err := ParseCodexAgent(items[i].Raw)
		if err != nil {
			return nil, fmt.Errorf("invalid Codex persona %s: %w", CodexAgentPath(items[i].Name), err)
		}
		items[i].Doc = doc
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return items, nil
}

// PrependArticlesBlock returns instructions with the Ship's Articles reminder
// prepended in a marker-delimited section. Idempotent: if the block is
// already present, the existing block is replaced verbatim.
//
// The block is a one-paragraph reminder pointing personas at the full
// canonical rules and the Brig's enforcement layers — not the entire
// Articles text (that would balloon every persona's context). Personas
// remain bound at three layers regardless of whether this block is
// spliced in; the block just makes the binding legible from inside the
// session.
func PrependArticlesBlock(instructions string) string {
	block := composeArticlesBlock()
	return spliceArticlesBlock(instructions, block)
}

func composeArticlesBlock() string {
	var b strings.Builder
	b.WriteString(brig.PromptStartMarker)
	b.WriteString("\n")
	b.WriteString("# The Ship's Articles\n")
	b.WriteString("\n")
	b.WriteString("You are bound by the Ship's Articles at `.shipmates/ARTICLES.md` (or, if that\n")
	b.WriteString("file is missing, `catalog/ARTICLES.md` in the shipmates repo). Fifteen rules,\n")
	b.WriteString("in two groups:\n\n")
	b.WriteString("- **Articles of Code (1-5)**: OWASP Top 10, OWASP LLM Top 10, CWE Top 25,\n")
	b.WriteString("  NIST SSDF (SP 800-218 v1.1), 12-Factor App. Follow them in every commit.\n")
	b.WriteString("- **Articles of Conduct (6-15)**: No Prod DB, No Destructive Git, No Secrets\n")
	b.WriteString("  in Commits, Verify Every Package, No Piped Execution, No Lies About\n")
	b.WriteString("  Failure, Respect the Freeze, Confirm Before Destroying, No Self-Escalation,\n")
	b.WriteString("  Stay Aboard. Kernel-enforced; violating one sends you to the Brig.\n\n")
	b.WriteString("If `.shipmates/freeze` exists, refuse all Write and Edit operations until an\n")
	b.WriteString("admiral clears it. If unsure whether an action is Article-safe, stop and ask.\n")
	b.WriteString(brig.PromptEndMarker)
	b.WriteString("\n")
	return b.String()
}

func spliceArticlesBlock(instructions, block string) string {
	start := strings.Index(instructions, brig.PromptStartMarker)
	end := strings.Index(instructions, brig.PromptEndMarker)
	if start == -1 || end == -1 || end < start {
		// No existing block — prepend with a blank-line separator.
		if strings.TrimSpace(instructions) == "" {
			return block
		}
		return block + "\n" + instructions
	}
	// Replace the block in place. Consume the trailing newline of the end
	// marker so re-runs are byte-stable.
	tail := end + len(brig.PromptEndMarker)
	if tail < len(instructions) && instructions[tail] == '\n' {
		tail++
	}
	head := start
	for head > 0 && (instructions[head-1] == ' ' || instructions[head-1] == '\t') {
		head--
	}
	before := strings.TrimRight(instructions[:head], "\n")
	after := strings.TrimLeft(instructions[tail:], "\n")
	if before == "" && after == "" {
		return block
	}
	if before == "" {
		return block + "\n" + after
	}
	if after == "" {
		return before + "\n\n" + block
	}
	return before + "\n\n" + block + "\n" + after
}

func InstalledPersonasAt(root string) ([]string, error) {
	items, err := CanonicalPersonaInventory(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, len(items))
	for i := range items {
		names[i] = items[i].Name
	}
	return names, nil
}
func InstalledPersonas() ([]string, error) { return InstalledPersonasAt(".") }
