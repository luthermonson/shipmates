// Package catalog provides read access to the persona catalog embedded in the
// shipmates binary. The on-disk source layout is:
//
//	catalog/<persona>/.claude/agents/<persona>.md   (the persona/subagent file)
//	catalog/<persona>/memory-seeds/*.md             (starter memory copied on install)
package catalog

import (
	"io/fs"
	"path"
	"sort"
)

// Catalog reads personas from an embedded (or any) filesystem rooted such that
// "catalog/..." is resolvable.
type Catalog struct {
	fsys fs.FS
}

// New wraps an fs.FS (typically the //go:embed catalog FS).
func New(fsys fs.FS) *Catalog {
	return &Catalog{fsys: fsys}
}

// Personas returns the sorted names of all personas in the catalog.
func (c *Catalog) Personas() ([]string, error) {
	entries, err := fs.ReadDir(c.fsys, "catalog")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// Has reports whether a persona exists in the catalog.
func (c *Catalog) Has(name string) bool {
	info, err := fs.Stat(c.fsys, path.Join("catalog", name))
	return err == nil && info.IsDir()
}

// AgentFile returns the raw bytes of a persona's subagent markdown file.
func (c *Catalog) AgentFile(name string) ([]byte, error) {
	return fs.ReadFile(c.fsys, path.Join("catalog", name, ".claude", "agents", name+".md"))
}

// MemorySeeds returns the starter memory files for a persona, keyed by base
// filename. Returns an empty map if the persona ships no seeds.
func (c *Catalog) MemorySeeds(name string) (map[string][]byte, error) {
	dir := path.Join("catalog", name, "memory-seeds")
	entries, err := fs.ReadDir(c.fsys, dir)
	if err != nil {
		// No seeds dir is fine — persona just starts blank.
		return map[string][]byte{}, nil
	}
	seeds := make(map[string][]byte, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		b, err := fs.ReadFile(c.fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		seeds[e.Name()] = b
	}
	return seeds, nil
}
