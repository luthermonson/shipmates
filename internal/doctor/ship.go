package doctor

import (
	"encoding/csv"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/luthermonson/shipmates/internal/project"
)

// checkShipSessionFiles reports the captain server's on-disk control files and,
// on Unix, whether their mode is tighter than 0600. These files carry the
// server's bearer token and session handles, so a group/other-readable copy is
// a leak — the same concern (and the same 0o077 test) internal/recovery applies
// to its journal.
//
// On Windows the mode argument buys nothing: Go synthesizes 0666/0777 bits for
// every writable file, so a mode check would reject files this very process
// wrote 0600. The honest report there is the DACL caveat, matching
// project.WritePrivateFile and recovery/journal_perm_windows.go — state the
// limitation rather than assert a mode.
func checkShipSessionFiles(e Env) []Result {
	files := []struct {
		name string
		path string
	}{
		{"server.token", project.TokenFile()},
		{"server.port", project.PortFile()},
		{"server.pid", project.PidFile()},
	}
	var out []Result
	for _, f := range files {
		info, err := os.Stat(f.path)
		if err != nil {
			out = append(out, Result{
				Name: f.name, Group: "Ship", Status: OK,
				Detail: "not present — no captain state on disk (normal when no mate has run)",
			})
			continue
		}
		out = append(out, sessionFilePerm(f.name, e.GOOS, info.Mode().Perm()))
	}
	return out
}

// sessionFilePerm is the pure perm-evaluation half of checkShipSessionFiles,
// split out so it can be unit-tested with synthetic modes across OSes without
// the Windows chmod hazard.
func sessionFilePerm(name, goos string, mode fs.FileMode) Result {
	r := Result{Name: name, Group: "Ship", Status: OK}
	if goos == "windows" {
		r.Detail = "present; on Windows the 0600 intent is not enforced by mode bits — access is governed by the inherited DACL of .shipmates (owner-only in a normal profile layout). See journal_perm_windows.go."
		return r
	}
	if mode&0o077 != 0 {
		r.Status = Warn
		r.Detail = fmt.Sprintf("present but mode %04o is group/other-accessible; expected 0600", mode)
		r.Hint = "chmod 600 " + filepathOf(name)
		return r
	}
	r.Detail = fmt.Sprintf("present, mode %04o", mode)
	return r
}

// filepathOf returns the on-disk path of a named session file for a hint.
func filepathOf(name string) string {
	switch name {
	case "server.token":
		return project.TokenFile()
	case "server.port":
		return project.PortFile()
	case "server.pid":
		return project.PidFile()
	}
	return name
}

// checkCaptainHealth does a cheap read-only /health probe of the captain server
// named by the on-disk port file. Not-running is informational, never a Fail:
// it is the normal state whenever no mate is live.
func checkCaptainHealth(e Env) []Result {
	b, err := os.ReadFile(project.PortFile())
	if err != nil {
		return []Result{{
			Name: "captain /health", Group: "Ship", Status: OK,
			Detail: "no server.port on disk — captain not running (normal unless a mate is live)",
		}}
	}
	port, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil {
		return []Result{{
			Name: "captain /health", Group: "Ship", Status: Warn,
			Detail: "server.port is present but not a number — likely a stale or corrupt file",
			Hint:   "remove " + project.PortFile() + " if no captain is running",
		}}
	}
	// A missing/empty token is not an error here: /health is unauthenticated,
	// and the token is only attached in case a future build gates it.
	token, _ := project.ReadAPIToken()
	if e.HealthProbe(port, token) {
		return []Result{{
			Name: "captain /health", Group: "Ship", Status: OK,
			Detail: fmt.Sprintf("captain responding on 127.0.0.1:%d", port),
		}}
	}
	return []Result{{
		Name: "captain /health", Group: "Ship", Status: OK,
		Detail: fmt.Sprintf("server.port names 127.0.0.1:%d but /health did not answer — captain not running (stale port file, or it has exited)", port),
	}}
}

// SupervisorProbe is the raw outcome of a per-OS supervisor-status query, split
// from its parsing so parseSupervisor can be unit-tested without invoking
// systemctl, launchctl or schtasks.
type SupervisorProbe struct {
	// Supported is false when this OS has no known supervisor query.
	Supported bool
	// Out is the combined stdout/stderr of the query.
	Out string
	// Err reports that the query command failed or returned non-zero.
	Err bool
}

// checkSupervisor reports whether the per-host ship supervisor is installed and
// enabled. Not-installed is informational (the supervisor is optional);
// installed-but-disabled is a Warn. It is never a Fail.
func checkSupervisor(e Env) []Result {
	return []Result{parseSupervisor(e.GOOS, e.Supervisor(e.GOOS))}
}

// parseSupervisor interprets a SupervisorProbe for the given OS. Kept pure and
// exhaustive per-OS so each parser can be table-tested against real command
// output samples.
func parseSupervisor(goos string, p SupervisorProbe) Result {
	if !p.Supported {
		return Result{
			Name: "supervisor (ship)", Group: "Ship", Status: OK,
			Detail: "no supervisor-status query is implemented for " + goos + " — reported as unknown rather than guessed",
		}
	}
	switch goos {
	case "linux":
		return parseSystemd(p)
	case "darwin":
		return parseLaunchctl(p)
	case "windows":
		return parseSchtasks(p)
	default:
		return Result{
			Name: "supervisor (ship)", Group: "Ship", Status: OK,
			Detail: "supervisor status unknown on " + goos,
		}
	}
}

// parseSystemd reads `systemctl --user is-enabled shipmates-ship.service`.
//
// systemctl reports a fixed vocabulary of unit-file states. Each recognized
// state maps to an honest verdict: only "enabled" is a clean OK; "masked" and
// "enabled-runtime" are installed-but-degraded (previously mis-mapped to the
// not-installed default, which reported them as absent); the other installed
// states remain Warns. A genuinely unrecognized line becomes a Warn "unknown
// state" — never a false OK claiming the unit is not installed.
func parseSystemd(p SupervisorProbe) Result {
	r := Result{Name: "supervisor (ship)", Group: "Ship"}
	s := strings.ToLower(strings.TrimSpace(p.Out))
	switch {
	case s == "enabled":
		r.Status = OK
		r.Detail = "systemd --user unit shipmates-ship.service is enabled"
	case s == "enabled-runtime":
		r.Status = Warn
		r.Detail = "systemd --user unit shipmates-ship.service is enabled only for the current boot (enabled-runtime) — it will not start after a reboot"
		r.Hint = "systemctl --user enable shipmates-ship.service"
	case s == "masked" || s == "masked-runtime":
		r.Status = Warn
		r.Detail = "systemd --user unit shipmates-ship.service is installed but " + s + " — it cannot be started until it is unmasked"
		r.Hint = "systemctl --user unmask shipmates-ship.service"
	case s == "disabled" || s == "linked" || s == "linked-runtime" ||
		s == "static" || s == "indirect" || s == "generated" ||
		s == "transient" || s == "alias":
		r.Status = Warn
		r.Detail = "systemd --user unit shipmates-ship.service is installed but " + s + " (will not start at login)"
		r.Hint = "systemctl --user enable --now shipmates-ship.service"
	case p.Err && (s == "" ||
		strings.Contains(s, "no such file") ||
		strings.Contains(s, "not found") ||
		strings.Contains(s, "could not be found") ||
		strings.Contains(s, "no unit")):
		// not-found: is-enabled exits non-zero and names a missing unit file.
		r.Status = OK
		r.Detail = "systemd --user unit shipmates-ship.service is not installed — optional (`shipmates ship install`)"
	default:
		// Anything else (e.g. "bad", "transient" variants we don't model, or a
		// future systemctl state) is reported honestly as unknown, never
		// assumed absent.
		r.Status = Warn
		r.Detail = "systemd --user unit shipmates-ship.service is in an unrecognized state (" + oneLine(p.Out) + ") — treated as unknown rather than assumed not installed"
	}
	return r
}

// parseLaunchctl reads `launchctl list` and looks for the ship agent label.
func parseLaunchctl(p SupervisorProbe) Result {
	r := Result{Name: "supervisor (ship)", Group: "Ship", Status: OK}
	if strings.Contains(p.Out, "cc.shipmates.ship") {
		r.Detail = "launchd user agent cc.shipmates.ship is loaded"
		return r
	}
	r.Detail = "launchd user agent cc.shipmates.ship is not loaded — optional (`shipmates ship install`)"
	return r
}

// parseSchtasks reads `schtasks /query /tn ShipmatesShip /fo CSV /v`.
//
// The old LIST form was scanned for the literal substring "disabled", but that
// word is localized on a non-English Windows, so a disabled task there silently
// read as installed-and-fine. The verbose CSV form exposes a dedicated
// "Scheduled Task State" column that we isolate and compare, which is robust to
// text elsewhere in the output. The state VALUE ("Enabled"/"Disabled") is itself
// localized, so a value we cannot map is reported as installed with the locale
// limitation stated in the Detail — honest rather than over-claiming enabled.
//
// The task NAME (ShipmatesShip) is our own literal and is not localized, so
// presence detection stays reliable across locales.
func parseSchtasks(p SupervisorProbe) Result {
	r := Result{Name: "supervisor (ship)", Group: "Ship", Status: OK}
	if !strings.Contains(strings.ToLower(p.Out), "shipmatesship") {
		r.Detail = "Scheduled Task ShipmatesShip is not installed — optional (`shipmates ship install`)"
		return r
	}

	state, ok := schtasksState(p.Out)
	switch {
	case !ok:
		// Couldn't isolate the state column (unexpected/older output format).
		// Installed, but do not over-claim the enable-state.
		r.Detail = "Scheduled Task ShipmatesShip is installed (its enable-state could not be read from the query output)"
	case strings.EqualFold(state, "disabled"):
		r.Status = Warn
		r.Detail = "Scheduled Task ShipmatesShip is installed but disabled (will not start at logon)"
		r.Hint = "schtasks /Change /TN ShipmatesShip /ENABLE"
	case strings.EqualFold(state, "enabled"):
		r.Detail = "Scheduled Task ShipmatesShip is installed and enabled"
	default:
		// A non-English Windows returns a localized state value we cannot map to
		// enabled/disabled. Report installed and state the limitation rather than
		// guess (the previous substring scan guessed wrong here).
		r.Detail = "Scheduled Task ShipmatesShip is installed; its enable-state reads as " + oneLine(state) + ", which this check cannot map to enabled/disabled on a non-English Windows locale"
	}
	return r
}

// schtasksState extracts the "Scheduled Task State" column value for the task
// from `schtasks ... /fo CSV /v` output. It returns ok=false when the output is
// not the expected verbose CSV (so the caller can avoid claiming a state). The
// column HEADER is matched in English; on a localized host the header will not
// match and the caller falls back to the honest "state unknown" path.
func schtasksState(out string) (string, bool) {
	rd := csv.NewReader(strings.NewReader(out))
	rd.FieldsPerRecord = -1 // schtasks rows are wide and occasionally uneven
	records, err := rd.ReadAll()
	if err != nil || len(records) < 2 {
		return "", false
	}
	col := -1
	for i, h := range records[0] {
		if strings.EqualFold(strings.TrimSpace(h), "Scheduled Task State") {
			col = i
			break
		}
	}
	if col < 0 {
		return "", false
	}
	// Prefer the data row that actually names our task; fall back to the first.
	for _, row := range records[1:] {
		if col >= len(row) {
			continue
		}
		for _, f := range row {
			if strings.Contains(strings.ToLower(f), "shipmatesship") {
				return strings.TrimSpace(row[col]), true
			}
		}
	}
	if col < len(records[1]) {
		return strings.TrimSpace(records[1][col]), true
	}
	return "", false
}

// productionSupervisor runs the read-only per-OS supervisor-status query. Every
// command here is a query (is-enabled / list / query) that only reads state; no
// branch installs, enables, starts or modifies anything. Only the branch
// matching runtime.GOOS is ever reached in production.
func productionSupervisor(goos string) SupervisorProbe {
	switch goos {
	case "linux":
		out, err := exec.Command("systemctl", "--user", "is-enabled", "shipmates-ship.service").CombinedOutput()
		return SupervisorProbe{Supported: true, Out: string(out), Err: err != nil}
	case "darwin":
		out, err := exec.Command("launchctl", "list").CombinedOutput()
		return SupervisorProbe{Supported: true, Out: string(out), Err: err != nil}
	case "windows":
		// /v /fo CSV exposes the "Scheduled Task State" column parseSchtasks
		// isolates, instead of scanning LIST text for a localized "disabled".
		out, err := exec.Command("schtasks", "/query", "/tn", "ShipmatesShip", "/v", "/fo", "CSV").CombinedOutput()
		return SupervisorProbe{Supported: true, Out: string(out), Err: err != nil}
	default:
		return SupervisorProbe{Supported: false}
	}
}
