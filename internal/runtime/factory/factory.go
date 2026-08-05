// Package factory maps a config-resolved runtime name onto a live
// runtime.Runtime, and is the one place that knows which concrete
// implementations exist.
//
// All three are wired here:
//
//   - claude: the `claude` CLI over stdio streaming. Session processes are
//     spawned through the containment watcher this factory resolves from
//     operator config.
//   - codex: the codex app-server transport. Its app-server child is spawned
//     through the same watcher, bound in as a supervisor by codex.Contain.
//   - openai: any OpenAI-compatible HTTP endpoint. It spawns no processes, so
//     there is nothing for a watcher to hold.
//
// Both process-spawning runtimes therefore share one containment story, and it
// is the portable one.
//
// Two seams sit alongside New because installing a persona or a memory hook
// is a file operation that `shipmates init`, `add` and `update` must be able
// to perform WITHOUT starting a runtime transport — codex would need an
// app-server, claude would need the CLI on PATH, openai would need an
// endpoint. See PersonaArtifact and InstallMemoryHook.
package factory

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/luthermonson/shipmates/internal/codexapp"
	"github.com/luthermonson/shipmates/internal/runtime"
	"github.com/luthermonson/shipmates/internal/runtime/claude"
	"github.com/luthermonson/shipmates/internal/runtime/codex"
	"github.com/luthermonson/shipmates/internal/runtime/config"
	"github.com/luthermonson/shipmates/internal/runtime/containment"
	"github.com/luthermonson/shipmates/internal/runtime/containment/none"
	"github.com/luthermonson/shipmates/internal/runtime/containment/watchdog"
	"github.com/luthermonson/shipmates/internal/runtime/openai"
)

// Options carries the per-invocation context a config file cannot: which
// directories this runtime works in. The resolved config supplies everything
// else.
type Options struct {
	// ProjectDir is the project root. Required.
	ProjectDir string
	// WorkingDir is where the runtime's tool calls originate. Defaults to
	// ProjectDir when empty — see internal/berth.
	WorkingDir string
}

func (o Options) workingDir() string {
	if o.WorkingDir != "" {
		return o.WorkingDir
	}
	return o.ProjectDir
}

// New constructs a Runtime from a fully resolved config plus the caller's
// directory context.
//
// Settings come from r.Settings, which internal/runtime/config has already
// guaranteed originated in the operator's own file — never in a project
// checkout. That guarantee is what makes it safe for this function to hand a
// binary path and an argument vector to os/exec.
func New(ctx context.Context, r config.Resolved, o Options) (runtime.Runtime, error) {
	if o.ProjectDir == "" {
		return nil, fmt.Errorf("factory: ProjectDir is required")
	}
	if !config.KnownRuntime(r.Runtime) {
		return nil, fmt.Errorf("factory: unknown runtime %q", r.Runtime)
	}
	watcher, limits, err := containmentFor(r.Containment)
	if err != nil {
		return nil, err
	}

	switch r.Runtime {
	case "claude":
		cfg := claude.Config{
			// Bind the operator's containment posture into the supervisor
			// claude spawns session processes through. A nil watcher would
			// silently mean "unbounded", so containmentFor never returns one.
			Supervisor: claude.Contain(watcher, limits),
		}
		if v, ok := stringSetting(r.Settings, "binary"); ok {
			cfg.Binary = v
		}
		if v, ok := stringSliceSetting(r.Settings, "default_args"); ok {
			cfg.DefaultArgs = v
		}
		return claude.New(cfg), nil

	case "codex":
		opts := codexapp.StartOptions{
			WorkingDirectory: o.workingDir(),
			// Bind the operator's containment posture into the supervisor the
			// app-server child is spawned through — the same watcher claude
			// gets. containmentFor never returns a nil watcher, so this is
			// never silently "unbounded".
			Supervisor: codex.Contain(watcher, limits),
		}
		// An explicit argv is the operator's escape hatch for a codex that is
		// not on PATH. Production callers leave it unset and get
		// "codex app-server --stdio".
		if v, ok := stringSliceSetting(r.Settings, "command"); ok {
			opts.Command = v
		}
		// codexapp's own startup wait is generous (over ten seconds), which is
		// right for a cold start on a loaded machine and wrong for an operator
		// who would rather fail fast. Let them say.
		if d, ok := durationSetting(r.Settings, "startup_timeout_ms"); ok {
			opts.StartupTimeout = d
		}
		rt, err := codex.New(ctx, opts)
		if err != nil {
			// Starting codex means starting a transport, which fails for
			// ordinary environmental reasons — not installed, wrong version,
			// no containment available. Say "not configured" so commands
			// point the operator at the docs instead of reporting a bug.
			return nil, &runtime.ErrNotConfigured{Runtime: "codex", Reason: err.Error()}
		}
		return rt, nil

	case "openai":
		// The openai runtime needs a base URL and a model, which only ever
		// come from settings, so an unconfigured selection is the common
		// case and deserves the actionable error.
		rt, err := openai.NewFromSettings(ctx, r.Settings)
		if err != nil {
			return nil, &runtime.ErrNotConfigured{Runtime: "openai", Reason: err.Error()}
		}
		return rt, nil
	}
	// Unreachable while config.Known and this switch agree — and
	// TestNew_CoversEveryKnownRuntime is what keeps them agreeing.
	return nil, fmt.Errorf("factory: runtime %q is known to config but not wired here", r.Runtime)
}

// Names returns the runtime names this factory can build. It is the same list
// config validates against, deliberately: a name shipmates accepts and then
// cannot build would be a worse error than a rejected one.
func Names() []string { return slices.Clone(config.Known) }

// Registry implements runtime.Factory over this package, for callers that
// want the interface rather than the functions.
type Registry struct {
	// Options is the directory context passed to New.
	Options Options
}

var _ runtime.Factory = Registry{}

// New implements runtime.Factory. Containment falls back to the default
// posture, since this entry point carries no resolved config; prefer the
// package-level New when the operator's containment choice is available.
func (reg Registry) New(ctx context.Context, name string, settings map[string]any) (runtime.Runtime, error) {
	return New(ctx, config.Resolved{
		Runtime:     name,
		Settings:    settings,
		Containment: config.Containment{Mode: config.DefaultContainmentMode},
	}, reg.Options)
}

// Names implements runtime.Factory.
func (Registry) Names() []string { return Names() }

// containmentFor translates the config-side containment block into a
// runtime-side Watcher plus Limits.
//
// Mode "cgroup" no longer exists and is REJECTED rather than degraded. It used
// to warn and fall back to the watchdog, which meant an operator asking for
// kernel-enforced limits could get polling ones and only find out by reading
// logs. Now that cgroup containment is gone from shipmates entirely there is
// nothing left to degrade to something else — accepting the name would be
// accepting a posture that cannot be delivered. config.Resolve rejects it
// first; this arm exists so a caller that builds a config.Containment by hand
// gets the same answer and the same instruction.
func containmentFor(c config.Containment) (containment.Watcher, containment.Limits, error) {
	limits := containment.Limits{
		MaxRSSBytes:     c.MemoryLimitMB * 1024 * 1024,
		MaxCPUSeconds:   float64(c.CPULimitSeconds),
		PollInterval:    time.Duration(c.PollIntervalMS) * time.Millisecond,
		GracefulTimeout: time.Duration(c.GracefulTimeoutMS) * time.Millisecond,
	}
	if c.MaxProcesses > 0 {
		limits.MaxProcesses = uint32(min(c.MaxProcesses, int64(^uint32(0))))
	}
	switch c.Mode {
	case "", config.DefaultContainmentMode:
		return watchdog.New(), limits, nil
	case "none":
		// Mode "none" means unbounded, so the limits go too. Returning the
		// watcher with limits attached would be the confusing half-answer.
		return none.New(), containment.Limits{}, nil
	case "cgroup":
		return nil, containment.Limits{}, fmt.Errorf("factory: %s", config.ErrCgroupModeRemoved)
	default:
		return nil, containment.Limits{}, fmt.Errorf("factory: unknown containment mode %q", c.Mode)
	}
}

// PersonaArtifactPath returns the project-relative path of the named
// runtime's own persona artifact, and whether it has one that shipmates
// tracks.
//
// The path is what `add` and `update` record in the install manifest, so an
// artifact is tracked, preserved when an operator hand-edits it, and removed
// with the persona. Only claude qualifies today: codex writes
// .codex/agents/<persona>.md but exposes no path/render pair outside a live
// Runtime, and openai reads the installed persona file rather than writing one
// of its own.
func PersonaArtifactPath(runtimeName, personaName string) (string, bool) {
	if runtimeName == "claude" {
		return claude.AgentPath(personaName), true
	}
	return "", false
}

// PersonaArtifact renders a canonical persona into the named runtime's own
// artifact: the project-relative path and its exact bytes.
//
// It exists separately from Runtime.InstallPersona for two reasons. Installing
// a persona is a file operation that `add` and `update` must perform without
// starting a runtime transport; and shipmates has to decide whether to write
// at all. Handing back the bytes is what lets the caller run them through the
// install manifest and leave an operator's edits alone. Runtime.InstallPersona
// renders through the same function, so both produce identical artifacts.
func PersonaArtifact(runtimeName string, p runtime.PersonaSpec) (path string, content []byte, ok bool, err error) {
	path, ok = PersonaArtifactPath(runtimeName, p.Name)
	if !ok {
		slog.Debug("runtime installs no persona artifact shipmates tracks", "runtime", runtimeName)
		return "", nil, false, nil
	}
	content, err = claude.RenderPersona(p)
	if err != nil {
		return "", nil, false, err
	}
	return path, content, true, nil
}

// InstallMemoryHook wires the named runtime's "read memory on session start"
// mechanism into projectDir.
//
// Dispatching by name here is what keeps a project free of the other
// runtimes' configuration: a codex-only project never grows a .claude/
// directory, and vice versa.
//
// An unknown runtime name is not an error. An operator who selected a runtime
// this build does not know about should not have `init` fail over a hook.
func InstallMemoryHook(runtimeName, projectDir string) error {
	switch runtimeName {
	case "claude":
		return claude.InstallMemoryHookAt(projectDir)
	case "codex":
		// Deliberately a no-op that writes nothing: codex carries memory in
		// the persona's developer instructions, which persona install already
		// renders.
		return codex.InstallMemoryHookAt(projectDir)
	case "openai":
		// The openai runtime builds its system prompt from the persona file
		// plus a memory digest on every StartSession, so there is no hook to
		// install and nothing on disk to write. Note this seam answers nil
		// where Runtime.InstallMemoryHook answers ErrUnsupported: the method
		// reports the capability truthfully, while this function reports
		// whether `init` has a file to write — and it does not, successfully.
		return nil
	default:
		slog.Debug("no memory-hook mechanism for runtime", "runtime", runtimeName)
		return nil
	}
}

// stringSetting reads a string out of the settings blob.
func stringSetting(settings map[string]any, key string) (string, bool) {
	v, ok := settings[key].(string)
	return v, ok && v != ""
}

// durationSetting reads a millisecond count out of the settings blob. YAML
// decodes an integer into int, and a value that came through JSON arrives as
// float64, so both are accepted. A non-positive value means "unset" rather
// than "zero", because a zero timeout is never what an operator meant.
func durationSetting(settings map[string]any, key string) (time.Duration, bool) {
	var ms int64
	switch v := settings[key].(type) {
	case int:
		ms = int64(v)
	case int64:
		ms = v
	case float64:
		ms = int64(v)
	default:
		return 0, false
	}
	if ms <= 0 {
		return 0, false
	}
	return time.Duration(ms) * time.Millisecond, true
}

// stringSliceSetting reads a list of strings out of the settings blob. YAML
// decodes a sequence into []any, so both that and a native []string are
// accepted; a list with a non-string element is dropped whole rather than
// silently truncated, because a half-applied argument vector is worse than
// none.
func stringSliceSetting(settings map[string]any, key string) ([]string, bool) {
	switch raw := settings[key].(type) {
	case []string:
		if len(raw) == 0 {
			return nil, false
		}
		return slices.Clone(raw), true
	case []any:
		out := make([]string, 0, len(raw))
		for _, v := range raw {
			s, ok := v.(string)
			if !ok {
				slog.Warn("ignoring runtime setting: every element must be a string", "key", key, "element_type", fmt.Sprintf("%T", v))
				return nil, false
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, false
		}
		return out, true
	default:
		return nil, false
	}
}
