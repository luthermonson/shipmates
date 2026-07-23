//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/luthermonson/shipmates/internal/m3provision"
)

func main() {
	if err := m3provision.ValidateInvocation(os.Args[1:], os.Environ(), os.Geteuid()); err != nil {
		fmt.Fprintln(os.Stderr, "shipmates-m3-provision:", err)
		os.Exit(2)
	}
	result, err := m3provision.ProvisionAt(m3provision.Layout{Base: m3provision.DefaultBase, Helper: m3provision.DefaultHelper, Unit: m3provision.DefaultUnit})
	if err != nil {
		fmt.Fprintln(os.Stderr, "shipmates-m3-provision: provisioning failed")
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		os.Exit(1)
	}
}
