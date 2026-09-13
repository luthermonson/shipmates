package doctor

import (
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
func parseSystemd(p SupervisorProbe) Result {
	r := Result{Name: "supervisor (ship)", Group: "Ship"}
	s := strings.ToLower(strings.TrimSpace(p.Out))
	switch {
	case s == "enabled":
		r.Status = OK
		r.Detail = "systemd --user unit shipmates-ship.service is enabled"
	case s == "disabled" || s == "linked" || s == "static" || s == "indirect" || s == "generated":
		r.Status = Warn
		r.Detail = "systemd --user unit shipmates-ship.service is installed but " + s + " (will not start at login)"
		r.Hint = "systemctl --user enable --now shipmates-ship.service"
	default:
		// not-found / "No such file" / any unrecognized non-zero result.
		r.Status = OK
		r.Detail = "systemd --user unit shipmates-ship.service is not installed — optional (`shipmates ship install`)"
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

// parseSchtasks reads `schtasks /query /tn ShipmatesShip /fo LIST`.
func parseSchtasks(p SupervisorProbe) Result {
	r := Result{Name: "supervisor (ship)", Group: "Ship", Status: OK}
	low := strings.ToLower(p.Out)
	if !strings.Contains(low, "shipmatesship") {
		r.Detail = "Scheduled Task ShipmatesShip is not installed — optional (`shipmates ship install`)"
		return r
	}
	if strings.Contains(low, "disabled") {
		r.Status = Warn
		r.Detail = "Scheduled Task ShipmatesShip is installed but disabled (will not start at logon)"
		r.Hint = "schtasks /Change /TN ShipmatesShip /ENABLE"
		return r
	}
	r.Detail = "Scheduled Task ShipmatesShip is installed"
	return r
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
		out, err := exec.Command("schtasks", "/query", "/tn", "ShipmatesShip", "/fo", "LIST").CombinedOutput()
		return SupervisorProbe{Supported: true, Out: string(out), Err: err != nil}
	default:
		return SupervisorProbe{Supported: false}
	}
}
