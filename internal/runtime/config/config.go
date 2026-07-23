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
}

// Resolved captures the final runtime selection after precedence rules.
type Resolved struct {
	Runtime  string
	Settings map[string]any
	// Source describes where the selection came from, for --explain / logs.
	Source string
}

// Defaults returns the built-in defaults used when neither config file
// exists.
func Defaults() File {
	return File{Runtime: "claude"}
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

// Resolve applies precedence: cliRuntime > project > user > defaults.
// Settings for the selected runtime are taken from the FIRST file that
// specified them (project then user), NOT merged, keeping the mental
// model simple.
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
		settings = s
	} else if s, ok := user.Runtimes[pick]; ok {
		settings = s
	}
	return Resolved{Runtime: pick, Settings: settings, Source: source}, nil
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
