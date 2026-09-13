package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/luthermonson/shipmates/internal/discord"
	"github.com/luthermonson/shipmates/internal/project"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/env"
	"github.com/luthermonson/shipmates/internal/runtime/openai"
	"github.com/luthermonson/shipmates/internal/ship"
)

// checkConfigFiles reports presence and clean parse of the four operator files
// under ~/.shipmates/. Absent is fine (an informational OK); the only failure
// is a file that EXISTS but does not parse, because that will break at runtime.
//
// Each file is parsed through its real loader so doctor and the runtime agree
// on what "malformed" means. The one exception is ship.yaml: ship.LoadConfig
// also treats a syntactically valid file that lists no usable project dirs as
// an error, which is a semantic complaint rather than a parse failure — so for
// the malformed check ship.yaml is unmarshaled into ship.Config directly
// (reusing the same type ship.LoadConfig decodes into).
func checkConfigFiles(e Env) []Result {
	home := e.homeDir()
	var out []Result

	if p, ok := config.UserPath(home); ok {
		out = append(out, operatorFile("~/.shipmates/config.yaml", p, func() error {
			_, err := config.LoadUser(home)
			return err
		}))
	}

	shipPath := filepath.Join(home, project.Dir, "ship.yaml")
	out = append(out, operatorFile("~/.shipmates/ship.yaml", shipPath, func() error {
		raw, err := os.ReadFile(shipPath)
		if err != nil {
			return err
		}
		var c ship.Config
		return yaml.Unmarshal(raw, &c)
	}))

	if p, ok := project.UserPersonasPath(home); ok {
		out = append(out, operatorFile("~/.shipmates/personas.yaml", p, func() error {
			_, err := project.LoadUserPersonas(home)
			return err
		}))
	}

	if p, ok := discord.DiscordConfigPath(home); ok {
		out = append(out, operatorFile("~/.shipmates/discord.yaml", p, func() error {
			_, err := discord.LoadFile(home)
			if errors.Is(err, discord.ErrNoConfigFile) {
				return nil
			}
			return err
		}))
	}

	return out
}

// operatorFile builds a Config-group result for one operator file: absent is an
// informational OK, present-and-clean is OK, present-but-unparseable is Fail.
func operatorFile(name, path string, parse func() error) Result {
	r := Result{Name: name, Group: "Config"}
	if _, err := os.Stat(path); err != nil {
		r.Status = OK
		r.Detail = "absent — optional, nothing configured here"
		return r
	}
	if err := parse(); err != nil {
		r.Status = Fail
		r.Detail = "present but does not parse: " + oneLine(err.Error())
		r.Hint = "fix the YAML in " + name + " — it will break at runtime"
		return r
	}
	r.Status = OK
	r.Detail = "present and parses cleanly"
	return r
}

// checkRuntimeSelection resolves which runtime this invocation would use
// (honoring --runtime > project config > user config > default, via the real
// env.Selector) and verifies the selected runtime can initialize.
//
// A selected openai runtime is validated with the runtime's own
// openai.ParseConfig: a missing base_url/model, or an api_key_env naming an
// unset variable, is a Fail because the runtime literally cannot run. For codex
// and claude, doctor reports availability with a read-only PATH probe rather
// than starting a transport — starting one would violate the read-only rule and
// spawn a process, so this is honestly lighter than the factory's live init.
func checkRuntimeSelection(e Env) []Result {
	home := e.homeDir()
	sel, err := (&env.Selector{UserHome: home}).Resolve(".", e.Runtime)
	if err != nil {
		return []Result{{
			Name: "runtime selection", Group: "Config", Status: Fail,
			Detail: "cannot resolve a runtime: " + oneLine(err.Error()),
			Hint:   "see docs/runtime-interface.md",
		}}
	}

	out := []Result{{
		Name: "runtime selection", Group: "Config", Status: OK,
		Detail: "runtime " + sel.Runtime + " (" + sel.Source + ")",
	}}

	switch sel.Runtime {
	case "openai":
		cfg, perr := openai.ParseConfig(sel.Settings)
		if perr != nil {
			out = append(out, Result{
				Name: "openai runtime config", Group: "Config", Status: Fail,
				Detail: "selected runtime cannot initialize: " + oneLine(perr.Error()),
				Hint:   "see docs/runtime-interface.md",
			})
			break
		}
		switch {
		case cfg.APIKeyEnv == "":
			out = append(out, Result{
				Name: "openai runtime config", Group: "Config", Status: OK,
				Detail: "base_url and model set; no api_key_env (endpoint treated as no-auth)",
			})
		case strings.TrimSpace(e.Getenv(cfg.APIKeyEnv)) == "":
			out = append(out, Result{
				Name: "openai runtime config", Group: "Config", Status: Fail,
				Detail: "api_key_env names " + cfg.APIKeyEnv + " but that variable is unset/empty — the selected runtime cannot authenticate",
				Hint:   "export " + cfg.APIKeyEnv + " (doctor never reads or prints its value)",
			})
		default:
			out = append(out, Result{
				Name: "openai runtime config", Group: "Config", Status: OK,
				Detail: "base_url and model set; api_key_env " + cfg.APIKeyEnv + " is set",
			})
		}
	case "codex":
		if _, cerr := e.LookPath("codex"); cerr != nil {
			out = append(out, Result{
				Name: "codex runtime", Group: "Config", Status: Warn,
				Detail: "selected runtime codex, but `codex` is not on PATH; a live session's app-server would fail to start (doctor does not start it — read-only)",
				Hint:   "install codex, or select the claude runtime",
			})
		} else {
			out = append(out, Result{
				Name: "codex runtime", Group: "Config", Status: OK,
				Detail: "selected runtime codex; `codex` found on PATH (doctor does not start the app-server — read-only)",
			})
		}
	case "claude":
		out = append(out, Result{
			Name: "claude runtime", Group: "Config", Status: OK,
			Detail: "selected runtime claude — binary availability reported under Toolchain",
		})
	}

	return out
}

// checkSecretEnv reports set/unset for each secret named by an env var in the
// operator's discord.yaml — never the value. The selected runtime's own secret
// (openai api_key_env) is covered by checkRuntimeSelection; the fleet token by
// checkFleet. A discord token unset is a Warn (that mate cannot start), never a
// Fail, since Discord is optional and not the selected runtime.
//
// personas.yaml is intentionally not scanned here: its schema
// (project.UserPersonaEntry) has no tokenEnv field, so there is no persona
// secret env to report. Reported honestly as "not applicable" rather than
// inventing a check.
func checkSecretEnv(e Env) []Result {
	home := e.homeDir()
	fc, err := discord.LoadFile(home)
	if err != nil {
		// Absent (ErrNoConfigFile) or malformed: nothing to report here.
		// Malformed discord.yaml is already surfaced by checkConfigFiles.
		return nil
	}
	if len(fc.Mates) == 0 {
		return nil
	}

	names := make([]string, 0, len(fc.Mates))
	for n := range fc.Mates {
		names = append(names, n)
	}
	sort.Strings(names)

	var out []Result
	for _, name := range names {
		tokenEnv := strings.TrimSpace(fc.Mates[name].TokenEnv)
		r := Result{Name: "discord token: " + name, Group: "Config"}
		switch {
		case tokenEnv == "":
			r.Status = Warn
			r.Detail = "mate " + name + " sets no tokenEnv in discord.yaml"
			r.Hint = "set mates." + name + ".tokenEnv to the NAME of the env var holding its bot token"
		case strings.TrimSpace(e.Getenv(tokenEnv)) == "":
			r.Status = Warn
			r.Detail = "tokenEnv " + tokenEnv + " is unset/empty — mate " + name + " cannot start"
			r.Hint = "export " + tokenEnv + " (doctor never reads or prints its value)"
		default:
			r.Status = OK
			r.Detail = "tokenEnv " + tokenEnv + " is set"
		}
		out = append(out, r)
	}
	return out
}
