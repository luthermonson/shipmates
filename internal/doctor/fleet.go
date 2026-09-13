package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/luthermonson/shipmates/internal/fleeturl"
	"github.com/luthermonson/shipmates/internal/permissions"
	"github.com/luthermonson/shipmates/internal/project"
)

// checkFleet runs only when this project is wired to a fleet (shipmates.yaml
// fleet.url non-empty); otherwise the whole Fleet group is omitted.
//
//   - fleet url: reused fleeturl.Validate — plaintext http/ws to a non-loopback
//     host is a Fail (the fleet token and the Admiral's deny list would travel
//     in cleartext, and the ship's own runtime already refuses it).
//   - fleet token: set/unset only, never the value.
//   - fleet policy cache: .shipmates/fleet-policy.json present and parseable.
//     A configured fleet with no cache is a Warn — on a restart while Fleet
//     Command is unreachable the ship runs without the fleet-wide deny list
//     until a fetch succeeds.
func checkFleet(e Env) []Result {
	conf, err := project.LoadConfig()
	if err != nil {
		return []Result{{
			Name: "fleet config", Group: "Fleet", Status: Fail,
			Detail: "shipmates.yaml does not parse: " + oneLine(err.Error()),
			Hint:   "fix the YAML in " + project.ConfigName,
		}}
	}
	url := strings.TrimSpace(conf.Fleet.URL)
	if url == "" {
		return nil // no fleet configured — omit the group
	}

	var out []Result

	if u, verr := fleeturl.Validate(url); verr != nil {
		out = append(out, Result{
			Name: "fleet url", Group: "Fleet", Status: Fail,
			Detail: oneLine(verr.Error()),
			Hint:   "use https:// (or a loopback host for local development)",
		})
	} else {
		out = append(out, Result{
			Name: "fleet url", Group: "Fleet", Status: OK,
			Detail: u.Redacted() + " (" + u.Scheme + ")",
		})
	}

	tokenEnv := strings.TrimSpace(conf.Fleet.TokenEnv)
	if tokenEnv == "" {
		tokenEnv = project.DefaultFleetTokenEnv
	}
	if strings.TrimSpace(e.Getenv(tokenEnv)) == "" {
		out = append(out, Result{
			Name: "fleet token", Group: "Fleet", Status: Warn,
			Detail: tokenEnv + " is unset/empty — the captain would connect to the fleet without auth",
			Hint:   "export " + tokenEnv + " (doctor never reads or prints its value)",
		})
	} else {
		out = append(out, Result{
			Name: "fleet token", Group: "Fleet", Status: OK,
			Detail: tokenEnv + " is set",
		})
	}

	cachePath := filepath.Join(project.Dir, "fleet-policy.json")
	if _, serr := os.Stat(cachePath); serr != nil {
		out = append(out, Result{
			Name: "fleet policy cache", Group: "Fleet", Status: Warn,
			Detail: "no " + cachePath + " yet — on a restart while Fleet Command is unreachable this ship has no fleet-wide deny list until a fetch succeeds",
			Hint:   "start the captain once while the fleet is reachable to populate the cache",
		})
	} else if b, rerr := os.ReadFile(cachePath); rerr != nil {
		out = append(out, Result{
			Name: "fleet policy cache", Group: "Fleet", Status: Warn,
			Detail: cachePath + " present but unreadable: " + oneLine(rerr.Error()),
		})
	} else {
		var pol permissions.FleetPolicy
		if json.Unmarshal(b, &pol) != nil {
			out = append(out, Result{
				Name: "fleet policy cache", Group: "Fleet", Status: Warn,
				Detail: cachePath + " present but is not valid JSON",
				Hint:   "it will be replaced on the next successful fleet-policy fetch",
			})
		} else {
			out = append(out, Result{
				Name: "fleet policy cache", Group: "Fleet", Status: OK,
				Detail: fmt.Sprintf("%s present (%d deny rule(s))", cachePath, len(pol.Deny)),
			})
		}
	}

	return out
}
