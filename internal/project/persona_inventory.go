package project

import (
	"fmt"
	"os"
	"sort"
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
