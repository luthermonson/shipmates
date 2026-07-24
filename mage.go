//go:build ignore

// Zero-install mage bootstrap: `go run mage.go <target>`.
package main

import (
	"os"

	"github.com/magefile/mage/mage"
)

func main() { os.Exit(mage.Main()) }
