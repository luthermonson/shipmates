package main

import "embed"

// catalogFS holds the entire persona catalog, baked into the binary at build
// time. The `all:` prefix is required so dotfiles/dotdirs (e.g. each persona's
// hidden catalog files are included when explicitly matched.
//
//go:embed all:catalog
var catalogFS embed.FS
