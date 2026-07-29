// Package config loads the shipmates runtime selection from
// .shipmates/config.yaml (project) with fallback to ~/.shipmates/config.yaml
// (user), overridden by a --runtime CLI flag. See
// docs/runtime-interface-plan.md for the schema.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

// File is the on-disk shape shared by project and user config files.
type File struct {
	// Runtime selects a runtime by name ("claude", "codex").
	Runtime string `yaml:"runtime,omitempty"`
	// Runtimes holds per-runtime settings. Unknown keys are preserved so
	// runtime implementations can consume their own settings blob without
	// this package needing to know them.
	Runtimes map[string]map[string]any `yaml:"runtimes,omitempty"`
	// Containment configures process-containment for spawned turns.
	Containment Containment `yaml:"containment,omitempty"`
}

// Containment is the on-disk shape for the containment: block.
//
//	containment:
//	  mode: watchdog        # watchdog | cgroup | none
//	  memory_limit_mb: 8192  # 0 = uncapped
//	  cpu_limit_seconds: 0
//	  max_processes: 0       # 0 = uncapped; kernel-enforced on Windows only
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

// Resolved captures the final runtime selection after precedence rules.
type Resolved struct {
	Runtime     string
	Settings    map[string]any
	Containment Containment
	// Source describes where the selection came from, for --explain / logs.
	Source string
}

// Defaults returns the built-in defaults used when neither config file
// exists. The default runtime is codex: the shipmates command surface is
// codex-native today, and defaulting to codex keeps `shipmates ask` on the
// existing dispatcher unless the operator explicitly selects claude via
// config or --runtime.
func Defaults() File {
	return File{Runtime: "codex"}
}

// LoadProject reads .shipmates/config.yaml under projectDir. Missing file
// returns a zero-value File with no error — that's the "no project
// override" case.
func LoadProject(projectDir string) (File, error) {
	return loadFile(filepath.Join(projectDir, ".shipmates", "config.yaml"))
}

// LoadUser reads ~/.shipmates/config.yaml. Missing file returns zero-value.
func LoadUser() (File, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return File{}, nil // treat as absent, not an error
	}
	return loadFile(filepath.Join(home, ".shipmates", "config.yaml"))
}

// executionKeys are the per-runtime settings that decide WHAT gets executed
// rather than which runtime is selected. They are honored from user config
// only — see the trust boundary described on Resolve.
var executionKeys = map[string]bool{"binary": true, "default_args": true}

// Resolve applies precedence: cliRuntime > project > user > defaults.
//
// Trust boundary: a project's .shipmates/config.yaml arrives with the
// checkout, so on a cloned repository it is attacker-controlled content that
// shipmates reads before the operator has reviewed anything. It may
// therefore select WHICH runtime a project uses ("this repo is a codex
// repo") but may not decide what that runtime executes or how it is
// contained. `binary` and `default_args` are taken from user config only,
// and containment likewise — otherwise a repository could point shipmates at
// an executable of its choosing, or set `containment: {mode: none}` to
// remove the resource bounds from every turn it runs.
//
// Non-execution settings for the selected runtime are still taken from the
// FIRST file that specified them (project then user), NOT merged, keeping
// the mental model simple.
func Resolve(cliRuntime string, project, user File) (Resolved, error) {
	defaults := Defaults()
	pick := ""
	source := "default"
	switch {
	case cliRuntime != "":
		pick = cliRuntime
		source = "--runtime flag"
	case project.Runtime != "":
		pick = project.Runtime
		source = "project config"
	case user.Runtime != "":
		pick = user.Runtime
		source = "user config"
	default:
		pick = defaults.Runtime
	}
	pick = strings.TrimSpace(strings.ToLower(pick))
	if pick == "" {
		return Resolved{}, errors.New("runtime resolution produced empty name")
	}
	var settings map[string]any
	if s, ok := project.Runtimes[pick]; ok {
		settings = stripExecutionKeys(s)
	} else if s, ok := user.Runtimes[pick]; ok {
		settings = s
	}
	// Execution settings come from user config regardless of which file
	// supplied the rest, so an operator's `binary` still applies to a project
	// that carries its own (untrusted) settings block.
	for k, v := range user.Runtimes[pick] {
		if !executionKeys[k] {
			continue
		}
		if settings == nil {
			settings = map[string]any{}
		}
		settings[k] = v
	}
	// Containment is user-config-only for the same reason: a checkout must
	// not be able to unbound the turns it runs.
	contain := user.Containment
	if contain.Mode == "" {
		contain.Mode = "watchdog" // sensible default: bounded but no privileges required
	}
	return Resolved{Runtime: pick, Settings: settings, Containment: contain, Source: source}, nil
}

// stripExecutionKeys copies s without the settings a project checkout is not
// trusted to supply. It copies rather than deleting in place because the
// caller's File may be reused.
func stripExecutionKeys(s map[string]any) map[string]any {
	if s == nil {
		return nil
	}
	out := make(map[string]any, len(s))
	for k, v := range s {
		if executionKeys[k] {
			continue
		}
		out[k] = v
	}
	return out
}

func loadFile(path string) (File, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return File{}, nil
		}
		return File{}, fmt.Errorf("read %s: %w", path, err)
	}
	var out File
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return File{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return out, nil
}
