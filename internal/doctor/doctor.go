// Package doctor is a read-only environment and configuration diagnostic for
// shipmates. It runs a registry of independent checks, prints them grouped with
// OK/WARN/FAIL markers, and reports whether any check FAILed so the CLI can
// exit non-zero for a pre-flight or CI hook.
//
// # Hard rules
//
// Every check is strictly read-only: it may stat files, read config, look a
// binary up on PATH, and issue cheap read-only probes (an HTTP GET /health, a
// per-OS "is the supervisor installed" query), but it never creates, modifies
// or deletes anything, never starts a long-lived process, and NEVER prints a
// secret value — a token or api-key is only ever reported as set or unset by
// the NAME of the environment variable that holds it.
//
// # Testability
//
// The machine-facing seams (home directory, PATH lookup, env getter, the
// /health probe and the per-OS supervisor query) are all fields on [Env], so a
// test constructs an Env pointing at a t.TempDir with fake probes and never
// touches the real machine. Project-relative state (.shipmates/, .claude/
// agents/, shipmates.yaml) is read from the current working directory exactly
// as every other shipmates command reads it, so a test uses t.Chdir to isolate
// it. Parsing that is awkward to drive through the real machine — file-mode
// evaluation and the per-OS supervisor output — is factored into pure
// functions ([sessionFilePerm], [parseSupervisor]) that are tested directly.
package doctor

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Status is a check outcome. Only Fail makes `shipmates doctor` exit non-zero;
// Warn is visible but non-fatal, so the command stays usable as a pre-flight
// hook without failing on optional or merely-degraded conditions.
type Status int

const (
	// OK is a healthy check, or an informational note about a normal state.
	OK Status = iota
	// Warn is a non-fatal problem worth surfacing.
	Warn
	// Fail means shipmates genuinely cannot work, or is actively insecure.
	// It is the only status that makes the command exit non-zero.
	Fail
)

func (s Status) String() string {
	switch s {
	case OK:
		return "OK"
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return "?"
	}
}

// Result is one check's finding. Detail explains the finding; Hint, when set,
// tells the operator what to do about it. Neither ever contains a secret.
type Result struct {
	Name   string
	Group  string
	Status Status
	Detail string
	Hint   string
}

// groupOrder is the stable print order of check groups.
var groupOrder = []string{"Toolchain", "Config", "Ship", "Fleet", "Personas"}

// Env carries every machine-facing seam a check needs, so checks are pure with
// respect to their inputs and a test can supply fakes. Project-relative reads
// (.shipmates/, .claude/agents/, shipmates.yaml) are deliberately NOT seams:
// they come from the current working directory, matching production, and a test
// isolates them with t.Chdir.
type Env struct {
	// Home is the parent of ~/.shipmates for operator-file checks. Empty
	// resolves via os.UserHomeDir.
	Home string
	// Runtime is the --runtime override for this invocation, "" if unset.
	Runtime string
	// GOOS selects OS-specific behavior (perms caveat, supervisor query).
	GOOS string
	// LookPath resolves a binary on PATH (exec.LookPath in production).
	LookPath func(string) (string, error)
	// Getenv reads an environment variable (os.Getenv in production). Used for
	// secret-presence checks so a test can set/unset without touching the real
	// process environment.
	Getenv func(string) string
	// HealthProbe reports whether a captain server answers GET /health on the
	// given loopback port using the given bearer token. Read-only.
	HealthProbe func(port int, token string) bool
	// Supervisor runs the per-OS, read-only supervisor-status query and returns
	// its raw outcome for parseSupervisor to interpret.
	Supervisor func(goos string) SupervisorProbe
}

// Production returns an Env wired to the real machine. home is usually "" (the
// real home directory); runtimeOverride is the --runtime flag value.
func Production(home, runtimeOverride string) Env {
	return Env{
		Home:        home,
		Runtime:     runtimeOverride,
		GOOS:        runtime.GOOS,
		LookPath:    exec.LookPath,
		Getenv:      os.Getenv,
		HealthProbe: productionHealthProbe,
		Supervisor:  productionSupervisor,
	}
}

// homeDir resolves the operator's home directory, honoring Env.Home.
func (e Env) homeDir() string {
	if e.Home != "" {
		return e.Home
	}
	h, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return h
}

// checks is the registry: the ordered set of check functions Run executes. Each
// returns zero or more Results; a check tagged to a group that is not
// configured (Fleet) returns nothing so its group is omitted from the output.
func checks() []func(Env) []Result {
	return []func(Env) []Result{
		checkToolchain,
		checkProjectConfig,
		checkConfigFiles,
		checkRuntimeSelection,
		checkSecretEnv,
		checkShipSessionFiles,
		checkCaptainHealth,
		checkSupervisor,
		checkFleet,
		checkPersonas,
	}
}

// Run executes every registered check against e and returns their combined
// results, in registry order.
func Run(e Env) []Result {
	var all []Result
	for _, c := range checks() {
		all = append(all, c(e)...)
	}
	return all
}

// Summary counts results by status.
type Summary struct {
	OK   int
	Warn int
	Fail int
}

// Failed reports whether any check failed — the exit-code condition.
func (s Summary) Failed() bool { return s.Fail > 0 }

// Summarize tallies results by status.
func Summarize(results []Result) Summary {
	var s Summary
	for _, r := range results {
		switch r.Status {
		case OK:
			s.OK++
		case Warn:
			s.Warn++
		case Fail:
			s.Fail++
		}
	}
	return s
}

// Render writes the grouped results and a summary line to w and returns the
// summary. Groups print in groupOrder; any unexpected group prints after, so a
// result is never silently dropped.
func Render(w io.Writer, results []Result) Summary {
	byGroup := map[string][]Result{}
	var extra []string
	seen := map[string]bool{}
	for _, g := range groupOrder {
		seen[g] = true
	}
	for _, r := range results {
		byGroup[r.Group] = append(byGroup[r.Group], r)
		if !seen[r.Group] {
			seen[r.Group] = true
			extra = append(extra, r.Group)
		}
	}

	fmt.Fprintln(w, "shipmates doctor — read-only environment & config diagnostic")
	for _, g := range append(append([]string{}, groupOrder...), extra...) {
		rs := byGroup[g]
		if len(rs) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s\n", g)
		for _, r := range rs {
			fmt.Fprintf(w, "  [%s] %s", marker(r.Status), r.Name)
			if r.Detail != "" {
				fmt.Fprintf(w, " — %s", r.Detail)
			}
			fmt.Fprintln(w)
			if r.Hint != "" {
				fmt.Fprintf(w, "         hint: %s\n", r.Hint)
			}
		}
	}

	s := Summarize(results)
	fmt.Fprintf(w, "\nsummary: %d OK, %d WARN, %d FAIL\n", s.OK, s.Warn, s.Fail)
	return s
}

// marker is the fixed-width status badge used in rendered output.
func marker(s Status) string {
	switch s {
	case OK:
		return " OK "
	case Warn:
		return "WARN"
	case Fail:
		return "FAIL"
	default:
		return " ?? "
	}
}

// oneLine flattens a multi-line message (a wrapped error) to a single line so a
// result stays on one row.
func oneLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// productionHealthProbe issues the real, read-only GET /health against the
// captain's loopback port, attaching the bearer token. It mirrors
// client.Healthy but is duplicated here (a) to keep the probe injectable and
// (b) because doctor also passes the token, so a future auth-gated /health
// still reports running rather than not-running.
func productionHealthProbe(port int, token string) bool {
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://127.0.0.1:%d/health", port), nil)
	if err != nil {
		return false
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	c := http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
