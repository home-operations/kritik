// Package web embeds the built kritik UI (see internal/web/src) so the
// kritik binary serves it without any external static-file dependency.
//
// dist/ is populated by `mise run ui-build` (see .mise/config.toml and the
// Dockerfile's ui stage); only dist/favicon.svg is committed so `go build`
// succeeds on a clean checkout before the UI has ever been built.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// FS returns the built UI's filesystem, rooted at dist so callers see
// index.html and friends directly rather than under a dist/ prefix.
func FS() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// fs.Sub only fails on a malformed path, and "dist" is a constant:
		// this can only happen if the embed above was ever changed without
		// updating this string too.
		panic(err)
	}

	return sub
}
