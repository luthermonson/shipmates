package doctor

import (
	"path/filepath"
	"testing"

	"github.com/luthermonson/shipmates/internal/project"
)

func writeFleetConfig(t *testing.T, proj, url string) {
	t.Helper()
	writeFile(t, filepath.Join(proj, "shipmates.yaml"),
		"fleet:\n  url: "+url+"\n  tokenEnv: FLEET_TOK\n")
}

func TestCheckFleetOmittedWhenUnconfigured(t *testing.T) {
	_, home := isolate(t)
	if res := checkFleet(baseEnv(home)); len(res) != 0 {
		t.Fatalf("no fleet configured should produce no results, got %+v", res)
	}
}

func TestCheckFleetURLValidation(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want Status
	}{
		{"https ok", "https://fleet.example.com", OK},
		{"loopback http ok", "http://127.0.0.1:9000", OK},
		{"plaintext non-loopback fail", "http://fleet.example.com", Fail},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			proj, home := isolate(t)
			writeFleetConfig(t, proj, tc.url)
			e := baseEnv(home)
			e.Getenv = getenvFrom(map[string]string{"FLEET_TOK": "x"})
			if got := findResult(t, checkFleet(e), "fleet url"); got.Status != tc.want {
				t.Fatalf("url %q: status = %v, want %v (%s)", tc.url, got.Status, tc.want, got.Detail)
			}
		})
	}
}

func TestCheckFleetTokenAndCache(t *testing.T) {
	proj, home := isolate(t)
	writeFleetConfig(t, proj, "https://fleet.example.com")

	// Token unset -> Warn; no policy cache -> Warn.
	e := baseEnv(home)
	res := checkFleet(e)
	if got := findResult(t, res, "fleet token"); got.Status != Warn {
		t.Fatalf("unset fleet token should Warn, got %v", got.Status)
	} else if containsSecret(got, project.DefaultFleetTokenEnv) {
		// The env-var NAME is fine to print; a *value* would not be. Here the
		// var is unset, so nothing to leak — this only guards the assertion.
	}
	if got := findResult(t, res, "fleet policy cache"); got.Status != Warn {
		t.Fatalf("missing policy cache should Warn, got %v (%s)", got.Status, got.Detail)
	}

	// Token set + valid policy cache -> both OK.
	e.Getenv = getenvFrom(map[string]string{"FLEET_TOK": "secretvalue"})
	writeFile(t, filepath.Join(proj, project.Dir, "fleet-policy.json"), `{"deny":["Bash(rm*)"]}`)
	res = checkFleet(e)
	tok := findResult(t, res, "fleet token")
	if tok.Status != OK {
		t.Fatalf("set fleet token should be OK, got %v", tok.Status)
	}
	if containsSecret(tok, "secretvalue") {
		t.Fatalf("fleet token result leaked the value: %+v", tok)
	}
	if got := findResult(t, res, "fleet policy cache"); got.Status != OK {
		t.Fatalf("valid policy cache should be OK, got %v (%s)", got.Status, got.Detail)
	}

	// Corrupt policy cache -> Warn.
	writeFile(t, filepath.Join(proj, project.Dir, "fleet-policy.json"), "{not json")
	if got := findResult(t, checkFleet(e), "fleet policy cache"); got.Status != Warn {
		t.Fatalf("corrupt policy cache should Warn, got %v (%s)", got.Status, got.Detail)
	}
}
