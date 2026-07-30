package commands

import (
	"fmt"
	"log/slog"

	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/env"
	"github.com/luthermonson/shipmates/internal/runtime/factory"
	"github.com/urfave/cli/v3"
)

// This file is the command layer's single point of contact with the runtime
// abstraction. Every command whose behavior depends on which runtime is in
// play offers --runtime and resolves it here; no command reads
// internal/runtime/config or builds a factory itself.
//
// # Why some commands refuse instead of adapting
//
// Selection is fully pluggable. The launch path is not, yet. The commands that
// start a session still exec the `claude` CLI with Claude Code's own session
// flags (--resume / --session-id / --agent, see internal/project.SessionLaunch)
// and internal/server drives it over Claude's stream-json protocol. Persona
// artifacts are likewise Claude's .claude/agents/*.md.
//
// So when a non-Claude runtime is selected, these commands say so and stop.
// That is a deliberate choice over the two alternatives: running claude anyway
// would ignore the operator's explicit instruction, and accepting --runtime
// while doing nothing with it is the failure mode this wiring exists to
// prevent. An error names the runtime, where the selection came from, and what
// is missing.

// projectDir is the root commands resolve runtime config against. Shipmates
// commands are CWD-relative throughout (project.LoadConfig reads
// ./shipmates.yaml), so runtime config is read the same way.
const projectDir = "."

// runtimeEnvVar is the environment spelling of --runtime.
const runtimeEnvVar = "SHIPMATES_RUNTIME"

// overrideSource is how the CLI layer describes a runtime override, replacing
// config.SourceOverride. It names both spellings on purpose: urfave/cli feeds
// the environment variable in through the same flag, so an operator told only
// "--runtime flag" would go looking through their shell history for something
// they never typed.
const overrideSource = "--runtime flag or " + runtimeEnvVar

// runtimeFlag is the shared --runtime flag. Every command that honors a
// runtime selection carries exactly this flag, so the spelling, the
// environment variable and the help text cannot drift between them.
//
// The flag is per-command rather than persistent on the root command. That
// costs a little ergonomics — it goes after the subcommand, `shipmates ask
// --runtime codex …` — and buys the property this whole seam is about: a
// command that does not honor a runtime selection does not accept one either.
func runtimeFlag() cli.Flag {
	return &cli.StringFlag{
		Name:    "runtime",
		Usage:   "agent runtime for this invocation: claude, codex or openai (default: .shipmates/config.yaml, then ~/.shipmates/config.yaml, then claude)",
		Sources: cli.EnvVars(runtimeEnvVar),
	}
}

// selector is the production runtime selector. A package-level value so tests
// can point it at a temporary home directory.
var selector = env.New()

// resolveRuntime resolves which runtime this invocation should use, applying
// --runtime > project config > user config > the claude default.
func resolveRuntime(c *cli.Command) (env.Selection, error) {
	sel, err := selector.Resolve(projectDir, c.String("runtime"))
	if err != nil {
		return env.Selection{}, err
	}
	if sel.Source == config.SourceOverride {
		sel.Source = overrideSource
	}
	slog.Debug("resolved runtime", "runtime", sel.Runtime, "source", sel.Source, "containment", sel.Containment.Mode)
	return sel, nil
}

// requireClaudeLaunch resolves the runtime and refuses if this command's
// Claude-native session-launch path cannot serve it.
func requireClaudeLaunch(c *cli.Command) (env.Selection, error) {
	sel, err := resolveRuntime(c)
	if err != nil {
		return sel, err
	}
	if sel.Runtime != "claude" {
		return sel, fmt.Errorf(
			"runtime %q is selected (%s), but %s launches sessions through Claude Code's CLI and session protocol, which cannot drive %s. "+
				"Use --runtime claude for this command, or see docs/runtime-interface.md",
			sel.Runtime, sel.Source, c.Name, sel.Runtime)
	}
	return sel, nil
}

// requireTrackedPersonaArtifact resolves the runtime and refuses if its persona
// artifact cannot be reconciled against the install manifest.
//
// The distinction matters: codex and openai both have persona formats and both
// write them from Runtime.InstallPersona. What they lack is the path-plus-bytes
// seam that lets `add` and `update` hash an artifact, compare it to the
// manifest, and leave an operator's hand-edits alone — without starting a
// transport to do it. These commands are built entirely around that
// reconciliation, so a runtime without the seam cannot be served here.
//
// The check asks the factory rather than comparing against "claude", so a
// runtime that grows the seam opens this gate without anyone remembering to
// come back here.
func requireTrackedPersonaArtifact(c *cli.Command) (env.Selection, error) {
	sel, err := resolveRuntime(c)
	if err != nil {
		return sel, err
	}
	if _, ok := factory.PersonaArtifactPath(sel.Runtime, "probe"); !ok {
		return sel, fmt.Errorf(
			"runtime %q is selected (%s), but shipmates cannot reconcile its persona artifact against the install manifest, which is all %s does. "+
				"Use --runtime claude for this command, or see docs/runtime-interface.md",
			sel.Runtime, sel.Source, c.Name)
	}
	return sel, nil
}
