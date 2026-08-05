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
	"strings"
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
		if e.IsDir() && c.isPersona(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// isPersona reports whether a catalog subdirectory is a persona rather than a
// shared resource directory.
//
// "Is a directory under catalog/" is not the same question. The catalog also
// holds charters/, commands/, routing/ and skills/, which are shared resources
// every persona draws on. Treating those as personas made `shipmates list`
// offer four things that cannot be added, and let `shipmates add charters` past
// validation only to fail deeper with a raw file-not-found:
//
//	read agent file: open catalog/charters/.claude/agents/charters.md: file does not exist
//
// The agent definition is the thing that actually makes a persona usable — it
// is what AgentFile reads and what gets vendored into a project — so its
// presence is the honest test, and it stays correct if more resource
// directories are added later.
func (c *Catalog) isPersona(name string) bool {
	_, err := fs.Stat(c.fsys, path.Join("catalog", name, ".claude", "agents", name+".md"))
	return err == nil
}

// Has reports whether a persona exists in the catalog.
// Has reports whether the catalog offers this persona.
//
// It asks the same question as Personas, deliberately: this is the gate `add`
// runs before vendoring, so a name that Has accepts but Personas never lists
// is exactly the mismatch that let `shipmates add charters` through.
func (c *Catalog) Has(name string) bool {
	info, err := fs.Stat(c.fsys, path.Join("catalog", name))
	return err == nil && info.IsDir() && c.isPersona(name)
}

// AgentFile returns the raw bytes of a persona's subagent markdown file.
func (c *Catalog) AgentFile(name string) ([]byte, error) {
	return fs.ReadFile(c.fsys, path.Join("catalog", name, ".claude", "agents", name+".md"))
}

// Commands returns the sorted names of slash commands in the catalog
// (catalog/commands/*.md). Empty if the catalog ships no commands.
func (c *Catalog) Commands() ([]string, error) {
	entries, err := fs.ReadDir(c.fsys, "catalog/commands")
	if err != nil {
		return nil, nil // no commands dir is fine
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			names = append(names, strings.TrimSuffix(e.Name(), ".md"))
		}
	}
	sort.Strings(names)
	return names, nil
}

// CommandFile returns a slash command's markdown (catalog/commands/<name>.md).
func (c *Catalog) CommandFile(name string) ([]byte, error) {
	return fs.ReadFile(c.fsys, path.Join("catalog", "commands", name+".md"))
}

// RoutingFile returns a routing-convention block by name (catalog/routing/<name>.md).
// Returns fs.ErrNotExist if there's no such routing template.
func (c *Catalog) RoutingFile(name string) ([]byte, error) {
	return fs.ReadFile(c.fsys, path.Join("catalog", "routing", name+".md"))
}

// CharterFile returns a charter template by name (catalog/charters/<name>.md).
func (c *Catalog) CharterFile(name string) ([]byte, error) {
	return fs.ReadFile(c.fsys, path.Join("catalog", "charters", name+".md"))
}

// PolicyFile returns the raw bytes of a persona's policy.yaml, if it ships one.
// Location: catalog/<persona>/policy.yaml. Returns fs.ErrNotExist when the
// persona has no per-persona policy — that's the common case; not every
// persona needs an overlay, and callers should treat missing as "no rules".
func (c *Catalog) PolicyFile(name string) ([]byte, error) {
	return fs.ReadFile(c.fsys, path.Join("catalog", name, "policy.yaml"))
}

// HasPolicyFile reports whether a persona ships a policy.yaml. Convenience for
// install/update code that wants to decide whether to vendor without paying
// the read cost twice.
func (c *Catalog) HasPolicyFile(name string) bool {
	_, err := fs.Stat(c.fsys, path.Join("catalog", name, "policy.yaml"))
	return err == nil
}

// ArticlesFile returns the canonical Ship's Articles document
// (catalog/ARTICLES.md) — the full text of the brig's fifteen rules.
// Vendored to .shipmates/ARTICLES.md on install so the persona prompt
// block's pointer resolves inside any project.
func (c *Catalog) ArticlesFile() ([]byte, error) {
	return fs.ReadFile(c.fsys, "catalog/ARTICLES.md")
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
