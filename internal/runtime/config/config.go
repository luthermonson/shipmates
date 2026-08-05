// Package config loads the shipmates runtime selection from
// .shipmates/config.yaml (project) and ~/.shipmates/config.yaml (user),
// overridden by a --runtime CLI flag.
//
// # Trust boundary
//
// The two files are NOT the same schema, and that asymmetry is the whole
// point of this package.
//
// A project's .shipmates/config.yaml arrives with the checkout. On a cloned
// repository it is attacker-controlled content that shipmates reads before
// the operator has reviewed anything. It may therefore answer exactly one
// question — "which runtime is this project written for?" — and nothing
// else. It may not name an executable, contribute arguments, or weaken the
// containment posture; a repository that could do any of those would get
// arbitrary code execution out of `git clone` plus any shipmates command.
//
// That restriction is enforced by [ProjectFile] having no fields for those
// things, rather than by filtering a shared struct. A filter is a denylist,
// and the first execution-shaped key someone adds without updating the
// denylist silently reopens the hole. A type cannot forget.
//
// Everything else — per-runtime settings (`runtimes:`) and containment
// (`containment:`) — is read from [UserFile] only, i.e. from
// ~/.shipmates/config.yaml, which is the operator's own file.
//
// See docs/runtime-interface.md for the schema and worked examples.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultRuntime is what a project with no configuration at all resolves to.
//
// It is "claude" because that is what shipmates has always launched: every
// spawn site, every persona artifact (.claude/agents/*.md), the SessionStart
// hook and the stream-json decoder are Claude Code's. Making the runtime
// pluggable must not move anybody off it — an operator opts in to a
// different runtime, they are never migrated by an upgrade.
const DefaultRuntime = "claude"

// Known lists the runtime names shipmates recognizes. Resolve rejects
// anything else, so neither a typo in the operator's config nor a hostile
// project checkout can name an arbitrary target.
var Known = []string{"claude", "codex", "openai"}

// KnownRuntime reports whether name is a recognized runtime name.
func KnownRuntime(name string) bool { return slices.Contains(Known, name) }

// ProjectFile is the subset of the config schema a project checkout is
// trusted to supply: the runtime name, and nothing else.
//
// Do not add fields here without re-reading the trust-boundary note on this
// package. Anything that influences what gets executed, with which
// arguments, or under what containment belongs on [UserFile].
// TestProjectFile_TrustedFieldsOnly guards this.
type ProjectFile struct {
	// Runtime selects a runtime by name; must be one of Known.
	Runtime string `yaml:"runtime,omitempty"`

	// Ignored lists top-level keys that were present in the file but that a
	// project checkout may not supply. Populated by LoadProject so the
	// operator gets told their project config was partly discarded instead
	// of silently wondering why it had no effect.
	Ignored []string `yaml:"-"`
}

// UserFile is the full config schema. Honored only from the operator's own
// ~/.shipmates/config.yaml.
type UserFile struct {
	// Runtime selects a runtime by name; must be one of Known.
	Runtime string `yaml:"runtime,omitempty"`
	// Runtimes holds per-runtime settings. Keys are runtime names; values
	// are an opaque blob handed to that runtime's constructor, so a runtime
	// implementation can grow settings without this package knowing them.
	Runtimes map[string]map[string]any `yaml:"runtimes,omitempty"`
	// Containment configures process containment for spawned turns.
	Containment Containment `yaml:"containment,omitempty"`
	// Brig configures the Ship's Articles subsystem (see internal/brig and
	// docs/brig.md). User config only, exactly like Runtimes and Containment:
	// security posture is the operator's decision, and a cloned repository
	// must not be able to un-sign the Articles for the operator's machine.
	// ProjectFile deliberately has no field for it, so a `brig:` block in a
	// project checkout lands in ProjectFile.Ignored and is warned about.
	Brig Brig `yaml:"brig,omitempty"`
}

// Brig is the on-disk shape of the brig: block. User config only — see the
// trust-boundary note on this package.
//
//	brig:
//	  enabled: true                       # false turns the Brig off entirely
//	  disabled_articles: [twelve-factor]  # waive individual Articles by handle
type Brig struct {
	// Enabled defaults to true when absent: installed means bound by the
	// Articles; opting out is the explicit act. A pointer so "absent" is
	// distinguishable from an explicit false.
	Enabled *bool `yaml:"enabled,omitempty"`
	// DisabledArticles lists Article handles (e.g. "no-piped-execution")
	// the operator waives while keeping the rest of the Brig. Unknown
	// handles are warned about and ignored by internal/brig.
	DisabledArticles []string `yaml:"disabled_articles,omitempty"`
}

// Containment is the on-disk shape for the containment: block. User config
// only — see the trust-boundary note on this package.
//
//	containment:
//	  mode: watchdog          # none | watchdog
//	  memory_limit_mb: 8192   # 0 = uncapped
//	  cpu_limit_seconds: 0    # 0 = uncapped
//	  max_processes: 0         # 0 = uncapped; kernel-enforced on Windows only
//	  poll_interval_ms: 500
//	  graceful_timeout_ms: 2000
type Containment struct {
	Mode              string `yaml:"mode,omitempty"`
	MemoryLimitMB     int64  `yaml:"memory_limit_mb,omitempty"`
	CPULimitSeconds   int64  `yaml:"cpu_limit_seconds,omitempty"`
	MaxProcesses      int64  `yaml:"max_processes,omitempty"`
	PollIntervalMS    int64  `yaml:"poll_interval_ms,omitempty"`
	GracefulTimeoutMS int64  `yaml:"graceful_timeout_ms,omitempty"`
}

// DefaultContainmentMode is the mode applied when the operator has not
// chosen one: the polling watchdog with no resource caps. That gives
// process-tree teardown (Unix process groups, Windows Job Objects) without
// requiring privileges or an install step, and imposes no limit nobody
// asked for.
const DefaultContainmentMode = "watchdog"

// ContainmentModes lists the accepted containment modes.
var ContainmentModes = []string{"none", "watchdog"}

// RemovedContainmentModes maps a mode shipmates once accepted onto the reason
// it no longer does. A removed mode gets its own error rather than falling
// into the generic "unknown mode" one: an operator who wrote it had it working
// at some point, and "unknown" would send them looking for a typo.
var RemovedContainmentModes = map[string]string{
	"cgroup": ErrCgroupModeRemoved,
}

// ErrCgroupModeRemoved is what an operator sees for `mode: cgroup`.
//
// It is a hard error, not a downgrade. The mode is not merely unimplemented —
// the Linux-only cgroup launcher behind it has been removed from shipmates, so
// there is no kernel-enforced posture left to ask for. Silently accepting the
// name and running the watchdog instead would tell an operator who wanted
// kernel enforcement that they had it.
const ErrCgroupModeRemoved = "containment mode \"cgroup\" has been removed: shipmates now contains every runtime with the portable watchdog (RSS/CPU sampling plus process-tree teardown on Linux, macOS and Windows). Use mode: watchdog — your memory_limit_mb, cpu_limit_seconds and other limits carry over unchanged"

// SourceOverride is the Resolved.Source value for a caller-supplied override —
// the cliRuntime argument to Resolve. It is deliberately layer-neutral: this
// package does not know whether the override arrived as a flag, an environment
// variable or something else, and naming a mechanism it cannot see would send
// an operator looking in the wrong place. The CLI layer replaces it with the
// spellings it actually offers.
const SourceOverride = "runtime override"

// Resolved captures the final selection after precedence rules.
type Resolved struct {
	// Runtime is the resolved runtime name, lowercased and trimmed. Always
	// one of Known.
	Runtime string
	// Settings is the per-runtime settings blob from user config, or nil.
	Settings map[string]any
	// Containment is the operator's containment posture.
	Containment Containment
	// Source describes where Runtime came from, for logs and diagnostics:
	// SourceOverride, "project config", "user config" or "default".
	Source string
	// IgnoredProjectKeys carries ProjectFile.Ignored through so callers can
	// warn once, at the point they resolve.
	IgnoredProjectKeys []string
}

// Resolve applies precedence — CLI flag > project config > user config >
// default — and returns the settings and containment posture, both of which
// come from user config regardless of which file supplied the name.
//
// An unrecognized runtime name is an error from whichever layer supplied it.
func Resolve(cliRuntime string, project ProjectFile, user UserFile) (Resolved, error) {
	pick, source := DefaultRuntime, "default"
	switch {
	case strings.TrimSpace(cliRuntime) != "":
		pick, source = cliRuntime, SourceOverride
	case strings.TrimSpace(project.Runtime) != "":
		pick, source = project.Runtime, "project config"
	case strings.TrimSpace(user.Runtime) != "":
		pick, source = user.Runtime, "user config"
	}
	pick = strings.ToLower(strings.TrimSpace(pick))
	if !KnownRuntime(pick) {
		return Resolved{}, fmt.Errorf("unknown runtime %q from %s (known: %s)",
			pick, source, strings.Join(Known, ", "))
	}

	// Settings are the operator's alone. A project checkout has no way to
	// reach this map: ProjectFile has no field for it.
	settings := user.Runtimes[pick]

	contain := user.Containment
	if strings.TrimSpace(contain.Mode) == "" {
		contain.Mode = DefaultContainmentMode
	}
	contain.Mode = strings.ToLower(strings.TrimSpace(contain.Mode))
	if gone, removed := RemovedContainmentModes[contain.Mode]; removed {
		return Resolved{}, errors.New(gone)
	}
	if !slices.Contains(ContainmentModes, contain.Mode) {
		return Resolved{}, fmt.Errorf("unknown containment mode %q in user config (known: %s)",
			contain.Mode, strings.Join(ContainmentModes, ", "))
	}

	return Resolved{
		Runtime:            pick,
		Settings:           settings,
		Containment:        contain,
		Source:             source,
		IgnoredProjectKeys: project.Ignored,
	}, nil
}

// ProjectPath returns the project config path under projectDir.
func ProjectPath(projectDir string) string {
	return filepath.Join(projectDir, ".shipmates", "config.yaml")
}

// UserPath returns the user config path under home. An empty home resolves
// via os.UserHomeDir; if that fails the second return is false and callers
// should treat user config as absent.
func UserPath(home string) (string, bool) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		home = h
	}
	return filepath.Join(home, ".shipmates", "config.yaml"), true
}

// projectOnlyKeys are the top-level keys a project checkout may set. Every
// other key found in a project file is reported in ProjectFile.Ignored.
var projectOnlyKeys = map[string]bool{"runtime": true}

// LoadProject reads .shipmates/config.yaml under projectDir. A missing file
// is not an error — it is the ordinary "this project has no opinion" case.
//
// The file is decoded into ProjectFile, which structurally cannot hold
// settings or containment, and separately scanned as a generic map so keys
// the project was not trusted to supply can be reported rather than
// vanishing without a trace.
func LoadProject(projectDir string) (ProjectFile, error) {
	path := ProjectPath(projectDir)
	raw, err := read(path)
	if err != nil || raw == nil {
		return ProjectFile{}, err
	}
	var out ProjectFile
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return ProjectFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var top map[string]any
	if err := yaml.Unmarshal(raw, &top); err != nil {
		return ProjectFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	for k := range top {
		if !projectOnlyKeys[strings.ToLower(k)] {
			out.Ignored = append(out.Ignored, k)
		}
	}
	sort.Strings(out.Ignored)
	return out, nil
}

// LoadUser reads ~/.shipmates/config.yaml. An empty home resolves via
// os.UserHomeDir. A missing file or an undiscoverable home is not an error.
func LoadUser(home string) (UserFile, error) {
	path, ok := UserPath(home)
	if !ok {
		return UserFile{}, nil
	}
	raw, err := read(path)
	if err != nil || raw == nil {
		return UserFile{}, err
	}
	var out UserFile
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return UserFile{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}

// read returns the file's bytes, or (nil, nil) when it does not exist.
func read(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return raw, nil
}
