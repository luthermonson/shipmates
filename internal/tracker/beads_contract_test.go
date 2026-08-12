//go:build !windows

// The beads leg of the shared tracker contract drives a `#!/bin/sh` stand-in
// for bd, mirroring internal/beads/client_test.go: it proves the adapter
// builds bounded argv and maps the Tracker vocabulary, while never executing
// the real bd (which boots an embedded Dolt engine). The real external
// contract is covered by the opt-in tests in internal/beads/live_test.go.
package tracker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/luthermonson/shipmates/internal/beads"
)

func init() {
	extraContractBackends = append(extraContractBackends, contractBackend{
		name: "beads",
		make: func(t *testing.T) Tracker {
			root := t.TempDir()
			if err := os.Mkdir(filepath.Join(root, ".beads"), 0o700); err != nil {
				t.Fatal(err)
			}
			script := filepath.Join(root, "bd")
			// Output shapes mirror real bd 1.1.2 (see internal/beads/parse_test.go):
			// create emits a JSON object with a fresh id, show emits a JSON array of
			// one record. Show for an id never created still answers — the shared
			// contract's Show(unknown) assertion is satisfied by the adapter's
			// validID guard plus the count file check below.
			body := `#!/bin/sh
root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
case "$1" in
create)
  n=0
  test -f "$root/count" && n=$(cat "$root/count")
  n=$((n+1))
  printf '%s' "$n" > "$root/count"
  printf '{"id":"ship-%s"}\n' "$n"
  ;;
prime) printf 'Use bd ready and bd show before working.\n' ;;
show)
  case "$2" in
    ship-*) printf '[{"id":"%s","status":"open"}]\n' "$2" ;;
    *) exit 1 ;;
  esac
  ;;
esac
`
			if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
			return &Beads{Client: &beads.Client{Root: root, Command: script, Timeout: time.Second}}
		},
	})
}
