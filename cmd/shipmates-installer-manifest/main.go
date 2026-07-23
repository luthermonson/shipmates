//go:build unix

// Command shipmates-installer-manifest emits the closed manifest for the
// embedded offline runtime assets. It performs no installation or host I/O.
//
// Unix-only: depends on internal/installer, which targets systemd + cgroup
// delegation.
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/luthermonson/shipmates/internal/installer"
)

func main() {
	m, err := installer.ManifestFor()
	if err != nil {
		fmt.Fprintln(os.Stderr, "shipmates-installer-manifest:", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(m); err != nil {
		os.Exit(1)
	}
}
