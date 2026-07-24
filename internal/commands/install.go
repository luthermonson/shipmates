package commands

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/luthermonson/shipmates/internal/brig"
	"github.com/luthermonson/shipmates/internal/catalog"
	"github.com/luthermonson/shipmates/internal/policy"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/urfave/cli/v3"
)

const emptyStrictPolicy = "version: 1\nallow: []\nask: []\ndeny: []\n"

var acquirePolicyWriteLock = project.AcquirePolicyWriteLock

// defaultConfig renders the starter shipmates.yaml, baking in the session
// prefix (the repo name) at init time. Leave sessionPrefix empty for no prefix.
func defaultConfig(sessionPrefix string) string {
	return fmt.Sprintf(`# shipmates.yaml — crew configuration for this project
# Personas are installed as Codex custom agents under .codex/agents/.
# Per-persona Codex model/effort or explicit command argv overrides go here.

# Prefix for per-persona session names (--name / --resume handles), written as
# this repo's name at init. Leave it empty (sessionPrefix: "") for no prefix, or
# change it to disambiguate two checkouts of the same repo / same-named projects.
sessionPrefix: %s

# The human operator is captain. The skipper is the conversational front door
# and execution lead used to prepare captain-approved voyages.
skipperPersona: skipper

# Available Codex models ordered from least to most capable. Sail starts at the
# first adequate tier and only moves right after a retry-safe failure.
modelLadder:
  - gpt-5.6-luna
  - gpt-5.6-terra
  - gpt-5.6-sol

# Per-persona overrides. Uncomment the crew key and add child keys to override
# Codex model or reasoning effort. Keeping it commented
# means there's no active empty crew key — so appending your own crew block
# won't create a duplicate top-level key (which yaml.v3 rejects).
# crew:
#   tester:
#     model: gpt-5.4
#   architect:
#     effort: high                       # low|medium|high|xhigh|max
# Routing substrate. Set to "github" to append GitHub issues/PRs routing
# conventions (claim-by-label, worktree-per-issue, Closes #n, verdict merge
# gate, cleanup ceremony) to every crew persona at install/update time. Leave
# empty to stay routing-agnostic (default). Run "shipmates update" after
# changing this to (re)compose installed personas.
# routing: github
# routingOptions:        # both default true (private-fleet conventions)
#   bylines: false       # "<persona> here" intros on gh comments — off for open source
#   labels: false        # persona-name labels as a work queue — off for open source
# routingOnBoot: true    # default false. true => personas get a one-line "run
#                        # /sync-routing on boot" instruction instead of the full
#                        # block embedded — keeps persona files tiny, one source
#                        # of truth. Requires the /sync-routing command (installed).

# Set true to commit per-persona memory (shared team knowledge) instead of
# keeping it gitignored (per-developer learnings).
sharedMemory: false
`, sessionPrefix)
}

// Init scaffolds shipmates into the current project.
func Init(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "init",
		Usage: "scaffold shipmates into the current project",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "crew", Usage: "comma-separated personas to add immediately"},
		},
		Action: func(ctx context.Context, c *cli.Command) error {
			m, err := project.LoadManifest()
			if err != nil {
				return err
			}
			if err := requireManifestV2(m, "init"); err != nil {
				return err
			}
			for _, d := range []string{
				project.Dir,
				filepath.Join(project.Dir, project.MemoryDirName),
				filepath.Join(project.Dir, project.SessionsDirName),
				project.PoliciesDir(),
				project.CodexAgentsDir,
			} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					return err
				}
			}
			if err := withPolicyWriteLock(func() error {
				return writeMissingPolicyFile(filepath.Join(project.Dir, "policy.yaml"), []byte(emptyStrictPolicy))
			}); err != nil {
				return err
			}

			if _, err := os.Stat(project.ConfigName); errors.Is(err, fs.ErrNotExist) {
				if err := os.WriteFile(project.ConfigName, []byte(defaultConfig(project.RepoName())), 0o644); err != nil {
					return err
				}
				slog.Info("wrote", "file", project.ConfigName)
			}

			if err := m.Save(); err != nil {
				return err
			}
			slog.Info("initialized shipmates project")

			if crew := c.String("crew"); crew != "" {
				for _, name := range strings.Split(crew, ",") {
					name = strings.TrimSpace(name)
					if name == "" {
						continue
					}
					if err := addPersona(cat, name); err != nil {
						return fmt.Errorf("add %s: %w", name, err)
					}
				}
			}
			return nil
		},
	}
}

// Add installs a canonical Codex persona, policy, and missing memory seeds.
func Add(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:      "add",
		Usage:     "install a Codex persona, policy, and missing memory seeds",
		ArgsUsage: "<persona>",
		Action: func(ctx context.Context, c *cli.Command) error {
			name := c.Args().First()
			if name == "" {
				return errors.New("usage: shipmates add <persona>")
			}
			return addPersona(cat, name)
		},
	}
}

// List shows catalog personas and which are installed.
func List(cat *catalog.Catalog) *cli.Command {
	return &cli.Command{
		Name:  "list",
		Usage: "list catalog personas and which are installed",
		Action: func(ctx context.Context, c *cli.Command) error {
			avail, err := cat.Personas()
			if err != nil {
				return err
			}
			for _, name := range avail {
				status := personaInstallStatus(name)
				fmt.Fprintf(c.Root().Writer, "%-14s %s\n", name, status)
			}
			return nil
		},
	}
}

// personaInstallStatus reads only the canonical Codex inventory.
func personaInstallStatus(name string) string {
	if _, err := os.Lstat(project.CodexAgentPath(name)); err == nil {
		if _, err := project.InstalledPersonaPath(name); err == nil {
			return "installed"
		}
		return "invalid-codex"
	}
	return ""
}

// composeAgent returns the persona's agent file with the project's routing
// block appended (wrapped in markers) when shipmates.yaml declares a routing
// layer. Composition is deterministic so `update` can recompose and diff
// against what was installed. With no routing declared (or an unknown routing
// name) the base file is returned unchanged.
func composeAgent(cat *catalog.Catalog, base []byte) ([]byte, error) {
	conf, err := project.LoadConfig()
	if err != nil {
		return nil, err
	}
	if conf.Routing == "" {
		return base, nil
	}

	var block []byte
	if conf.RoutingOnBoot {
		// Slash-command-on-boot: keep persona files tiny; the worker loads the
		// rules at session start via the /sync-routing command.
		block = []byte("On session start, immediately run `/sync-routing` to load this project's routing conventions, then proceed.")
	} else {
		b, err := renderRoutingBlock(cat)
		if err != nil {
			return nil, err
		}
		if b == nil { // unknown routing name — leave the base unchanged
			return base, nil
		}
		block = b
	}

	var b bytes.Buffer
	b.Write(bytes.TrimRight(base, "\n"))
	fmt.Fprintf(&b, "\n\n<!-- shipmates:routing:%s -->\n", conf.Routing)
	b.Write(bytes.TrimRight(block, "\n"))
	fmt.Fprintf(&b, "\n<!-- /shipmates:routing:%s -->\n", conf.Routing)
	return b.Bytes(), nil
}

// renderRoutingBlock renders the active routing template (per shipmates.yaml's
// routing: + routingOptions:). Returns nil if no routing is configured or the
// named template doesn't exist. Single source of truth for the block, shared by
// composeAgent (full mode) and `shipmates routing show` / the /sync-routing
// command.
func renderRoutingBlock(cat *catalog.Catalog) ([]byte, error) {
	conf, err := project.LoadConfig()
	if err != nil {
		return nil, err
	}
	if conf.Routing == "" {
		return nil, nil
	}
	raw, err := cat.RoutingFile(conf.Routing)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("routing").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse routing template %s: %w", conf.Routing, err)
	}
	bylines, labels := conf.RoutingOptions.Resolved()
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, struct{ Bylines, Labels, Beads bool }{bylines, labels, beadsWorkspace()}); err != nil {
		return nil, fmt.Errorf("render routing template %s: %w", conf.Routing, err)
	}
	return collapseBlankLines(rendered.Bytes()), nil
}

// attachInboxIgnorePattern is the path shipmates adds to .gitignore so a
// binary attach doesn't accidentally get committed. Kept as a package-level
// constant so both the installer and the eventual `shipmates update` share
// the exact string.
const attachInboxIgnorePattern = ".shipmates/inbox/"

// ensureAttachGitignore makes sure the project's root .gitignore ignores the
// attach inbox. If .gitignore doesn't exist yet we create one containing just
// the inbox pattern; if it exists we append the pattern only when it isn't
// already present, preserving whatever the user has above.
func ensureAttachGitignore() error {
	const path = ".gitignore"
	existing, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		// Fresh file — write a minimal shipmates-managed header plus the
		// pattern. Leaving the file otherwise empty is polite: the user's
		// own ignores will land underneath without conflict.
		content := "# shipmates: keep inbound binary attachments out of git\n" +
			attachInboxIgnorePattern + "\n"
		return os.WriteFile(path, []byte(content), 0o644)
	}
	if err != nil {
		return err
	}
	if gitignoreContainsPattern(existing, attachInboxIgnorePattern) {
		return nil
	}
	// Append with a leading newline only when the existing file doesn't
	// already end with one — avoids gluing the pattern onto the last user
	// entry.
	buf := bytes.TrimRight(existing, "\r\n")
	buf = append(buf, '\n', '\n')
	buf = append(buf, "# shipmates: keep inbound binary attachments out of git\n"...)
	buf = append(buf, attachInboxIgnorePattern...)
	buf = append(buf, '\n')
	return os.WriteFile(path, buf, 0o644)
}

// gitignoreContainsPattern reports whether a .gitignore body already includes
// the given pattern on a line of its own (ignoring surrounding whitespace and
// comment lines). Cheap linear scan — the file is always tiny.
func gitignoreContainsPattern(body []byte, pattern string) bool {
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if line == pattern {
			return true
		}
	}
	return false
}

// beadsWorkspace reports whether the project is beads-enabled (.beads/ in the
// project root). Routing templates use it to include the gh ↔ beads seam
// conventions only where a graph actually exists. Re-run `shipmates update`
// (or `routing apply`) after `bd init` to recompose personas with the section.
func beadsWorkspace() bool {
	st, err := os.Stat(".beads")
	return err == nil && st.IsDir()
}

// collapseBlankLines squeezes runs of 3+ newlines down to 2, cleaning up the
// gaps that template conditionals leave behind.
func collapseBlankLines(b []byte) []byte {
	s := string(b)
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return []byte(strings.TrimSpace(s) + "\n")
}

// applyRouting upserts the active routing block into an arbitrary persona file
// (custom or catalog): it strips any existing shipmates routing block, then
// re-appends the current one. Idempotent — re-run to re-sync after the upstream
// routing template changes.
func applyRouting(cat *catalog.Catalog, content []byte) ([]byte, error) {
	return composeAgent(cat, stripRoutingBlock(content))
}

// stripRoutingBlock removes a previously-composed <!-- shipmates:routing:* -->
// … <!-- /shipmates:routing:* --> block (and its trailing newline) if present,
// leaving everything else untouched.
func stripRoutingBlock(b []byte) []byte {
	s := string(b)
	start := strings.Index(s, "<!-- shipmates:routing:")
	endMark := "<!-- /shipmates:routing:"
	end := strings.Index(s, endMark)
	if start < 0 || end < 0 || end < start {
		return b
	}
	before := strings.TrimRight(s[:start], "\n")
	after := ""
	if nl := strings.IndexByte(s[end:], '\n'); nl >= 0 {
		after = s[end+nl+1:]
	}
	out := before
	if strings.TrimSpace(after) != "" {
		out = before + "\n\n" + strings.TrimLeft(after, "\n")
	}
	return []byte(strings.TrimRight(out, "\n") + "\n")
}

// addPersona is the shared install routine used by `add` and `init --crew`.
func addPersona(cat *catalog.Catalog, name string) error {
	if !cat.Has(name) {
		return fmt.Errorf("unknown persona %q", name)
	}
	// The directory descriptor is the synchronization object. Creating the
	// control directory does not mutate policy; acquisition below reopens it
	// without following symlinks before any canonical artifact is touched.
	if err := os.MkdirAll(project.Dir, 0o755); err != nil {
		return err
	}
	m, err := project.LoadManifest()
	if err != nil {
		return err
	}
	if err := requireManifestV2(m, "add"); err != nil {
		return err
	}
	return withPolicyWriteLock(func() error { return addPersonaLocked(cat, name, m) })
}

func addPersonaLocked(cat *catalog.Catalog, name string, m *project.Manifest) error {

	base, err := cat.AgentFile(name)
	if err != nil {
		return fmt.Errorf("read agent file: %w", err)
	}
	agent, err := composeAgent(cat, base)
	if err != nil {
		return err
	}

	// Render the canonical Codex artifact directly from the catalog persona;
	// no alternate runtime artifact is created as an intermediate.
	fm, body := splitPersona(agent)
	codexAgent := []byte(renderCodex(fm, body))
	codexPath := project.CodexAgentPath(name)
	if err := reconcileAddFile(m, codexPath, codexAgent, "Codex agent"); err != nil {
		return err
	}

	// Seed memory — but never overwrite existing memory (it's sacred).
	memDir := project.MemoryDir(name)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		return err
	}
	seeds, err := cat.MemorySeeds(name)
	if err != nil {
		return err
	}
	for fname, content := range seeds {
		mp := filepath.Join(memDir, fname)
		if _, err := os.Stat(mp); err == nil {
			slog.Debug("memory exists, leaving untouched", "file", mp)
			continue
		}
		if err := os.WriteFile(mp, content, 0o644); err != nil {
			return err
		}
		slog.Info("seeded memory", "file", mp)
	}

	// Vendor the persona's policy overlay (permissions.allow/ask/deny) into
	// .shipmates/policies/<persona>.yaml. Same "don't clobber user edits"
	// discipline as installCommands: if the file already exists and its
	// content diverges from what shipmates last installed, leave it alone.
	pol, err := catalogM5Policy(cat, name)
	if err != nil {
		return fmt.Errorf("read policy for %s: %w", name, err)
	}
	if err := reconcileAddFile(m, project.PolicyPath(name), pol, "policy"); err != nil {
		return err
	}

	// Stamp the Brig's Articles reminder block into the persona's overlay.
	// The block is commented YAML — see brig.MergeInto's docstring — so it
	// never activates a rule the operator didn't opt into, but it puts the
	// full Articles catalogue one file-open away from every persona.
	if err := installBrigBlock(cat, name); err != nil {
		slog.Warn("brig: could not stamp Articles block", "persona", name, "err", err)
	}

	return m.Save()
}

// installBrigBlock reads the embedded Brig template and merges it into the
// persona's overlay. Missing template is a soft failure — the persona still
// works, it just doesn't get the Articles reminder yet.
func installBrigBlock(cat *catalog.Catalog, name string) error {
	template, err := cat.BrigPolicyTemplate()
	if err != nil {
		return fmt.Errorf("read Brig template: %w", err)
	}
	return brig.MergeInto(project.PolicyPath(name), template)
}

// catalogM5Policy returns a catalog overlay only when it satisfies the strict
// M5 v1 contract. Older catalogs shipped incompatible permission lists; those
// are not policy inputs and are represented by the fail-closed empty overlay.
func catalogM5Policy(cat *catalog.Catalog, name string) ([]byte, error) {
	b, err := cat.PolicyFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return []byte(emptyStrictPolicy), nil
	}
	if err != nil {
		return nil, err
	}
	sources := []policy.Source{
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerProject, Path: ".shipmates/policy.yaml", Present: true}, Bytes: []byte(emptyStrictPolicy)},
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerProjectLocal, Path: ".shipmates/policy.local.yaml", Present: false}},
		{Descriptor: policy.SourceDescriptor{Layer: policy.LayerPersona, Path: project.PolicyPath(name), Present: true}, Bytes: b},
	}
	if snapshot, _ := policy.Parse(name, "catalog", sources); snapshot == nil {
		return []byte(emptyStrictPolicy), nil
	}
	return b, nil
}

func withPolicyWriteLock(mutate func() error) error {
	lock, err := acquirePolicyWriteLock(".")
	if err != nil {
		return err
	}
	mutateErr := mutate()
	closeErr := lock.Close()
	return errors.Join(mutateErr, closeErr)
}

func writeMissingPolicyFile(path string, content []byte) error {
	if _, err := os.Lstat(path); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect base policy: %w", err)
	}
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write base policy: %w", err)
	}
	slog.Info("installed base policy", "path", path)
	return nil
}

// reconcileAddFile installs a missing managed file, but applies the same
// baseline/conflict rules as update when a target already exists. Add has no
// conflict-acceptance flag, so conflicts always preserve local content.
func reconcileAddFile(m *project.Manifest, path string, shipped []byte, label string) error {
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		if err := writeManaged(path, shipped); err != nil {
			return err
		}
		m.Files[path] = project.SHA(shipped)
		slog.Info("installed "+label, "path", path)
		return nil
	} else if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}

	st := &updateState{in: bufio.NewScanner(strings.NewReader(""))}
	return reconcileFile(m, st, path, shipped, label)
}
