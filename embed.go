package main

import "embed"

// catalogFS holds the entire persona catalog, baked into the binary at build
// time. The `all:` prefix is required so dotfiles/dotdirs (e.g. each persona's
// .claude/ directory) are included — //go:embed skips them otherwise.
//
//go:embed all:catalog
var catalogFS embed.FS
