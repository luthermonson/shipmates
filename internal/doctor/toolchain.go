package doctor

import (
	"errors"
	"io/fs"
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
	configured, why, cfgErr := beadsConfigured()
	switch {
	case bdErr == nil:
		out = append(out, Result{Name: "bd (Beads)", Group: "Toolchain", Status: OK, Detail: "found on PATH"})
	case cfgErr != nil:
		// shipmates.yaml is malformed. That single fault is FAILed once, under
		// the Config group, by checkProjectConfig — so here we neither re-report
		// the parse error nor let it collapse into a misleading "Beads is not
		// configured". We simply cannot tell whether Beads is configured.
		out = append(out, Result{
			Name: "bd (Beads)", Group: "Toolchain", Status: OK,
			Detail: "not found on PATH; whether Beads is configured is unknown because " + project.ConfigName + " does not parse (see the Config group)",
		})
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
//
// A parse error is returned rather than swallowed into a false "not configured":
// checkProjectConfig FAILs on a malformed shipmates.yaml under the Config group,
// and the caller uses this error to avoid contradicting it with a bogus
// "Beads is not configured" verdict derived from a file that does not parse.
// An absent file is the ordinary "no project config" case and is not an error.
func beadsConfigured() (bool, string, error) {
	if beads.Workspace(".") {
		return true, "an initialized .beads workspace is present", nil
	}
	raw, err := os.ReadFile(filepath.Join(".", project.ConfigName))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, "", nil
		}
		return false, "", err
	}
	var cfg struct {
		Voyage struct {
			Tracker string `yaml:"tracker"`
		} `yaml:"voyage"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return false, "", err
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Voyage.Tracker), "beads") {
		return true, "voyage.tracker: beads in " + project.ConfigName, nil
	}
	return false, "", nil
}
