//go:build linux

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/luthermonson/shipmates/internal/m3runtime"
)

func main() {
	if err := m3runtime.ValidateInvocation(os.Args[1:], os.Environ()); err != nil {
		fmt.Fprintln(os.Stderr, "shipmates-m3-qualifier-run:", err)
		os.Exit(2)
	}
	if err := m3runtime.Run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "shipmates-m3-qualifier-run: prerequisite_failed")
		os.Exit(1)
	}
}
