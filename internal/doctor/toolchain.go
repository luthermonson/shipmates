package doctor

import (
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/beads"
	"github.com/luthermonson/shipmates/internal/project"
)

// checkToolchain verifies the external binaries shipmates depends on.
//
//   - claude: the hard dependency — every Claude-backed mate launches through
//     it, so its absence is the one toolchain Fail.
//   - git: Warn if missing (routing and berth features shell out to it).
//   - bd (Beads): only meaningful when Beads is configured; Warn when
//     configured-but-missing, otherwise an informational OK (Beads is optional).
func checkToolchain(e Env) []Result {
	var out []Result

	if _, err := e.LookPath("claude"); err != nil {
		out = append(out, Result{
			Name: "claude CLI", Group: "Toolchain", Status: Fail,
			Detail: "not found on PATH — shipmates launches every Claude-backed mate through the `claude` CLI",
			Hint:   "install Claude Code and put `claude` on PATH (https://docs.claude.com/claude-code)",
		})
	} else {
		out = append(out, Result{
			Name: "claude CLI", Group: "Toolchain", Status: OK,
			Detail: "found on PATH (presence only — verifying Claude Code auth would require launching a session, which is out of scope for a read-only probe)",
		})
	}

	if _, err := e.LookPath("git"); err != nil {
		out = append(out, Result{
			Name: "git", Group: "Toolchain", Status: Warn,
			Detail: "not found on PATH",
			Hint:   "install git — routing and berth features shell out to it",
		})
	} else {
		out = append(out, Result{Name: "git", Group: "Toolchain", Status: OK, Detail: "found on PATH"})
	}

	_, bdErr := e.LookPath("bd")
	configured, why := beadsConfigured()
	switch {
	case bdErr == nil:
		out = append(out, Result{Name: "bd (Beads)", Group: "Toolchain", Status: OK, Detail: "found on PATH"})
	case configured:
		out = append(out, Result{
			Name: "bd (Beads)", Group: "Toolchain", Status: Warn,
			Detail: "not found on PATH, but Beads is configured (" + why + ")",
			Hint:   "install bd (https://github.com/gastownhall/beads), or set voyage.tracker: markdown",
		})
	default:
		out = append(out, Result{
			Name: "bd (Beads)", Group: "Toolchain", Status: OK,
			Detail: "not installed, and Beads is not configured — informational; Beads is optional",
		})
	}

	return out
}

// beadsConfigured reports whether this project uses Beads: either an
// initialized .beads workspace (reusing beads.Workspace) or an explicit
// voyage.tracker: beads in shipmates.yaml. The tracker field is read the same
// way internal/commands.selectVoyageTracker reads it (that loader is
// unexported), against the same shipmates.yaml.
func beadsConfigured() (bool, string) {
	if beads.Workspace(".") {
		return true, "an initialized .beads workspace is present"
	}
	raw, err := os.ReadFile(filepath.Join(".", project.ConfigName))
	if err != nil {
		return false, ""
	}
	var cfg struct {
		Voyage struct {
			Tracker string `yaml:"tracker"`
		} `yaml:"voyage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return false, ""
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Voyage.Tracker), "beads") {
		return true, "voyage.tracker: beads in " + project.ConfigName
	}
	return false, ""
}
