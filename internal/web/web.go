// Package web serves the embedded dashboard. The entire frontend is compiled
// into the binary via go:embed so Pulsebox has zero runtime asset dependencies
// and works in air-gapped / private-registry environments.
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Handler returns an http.Handler that serves the dashboard's static assets.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err) // embedded FS is known-good at build time
	}
	return http.FileServer(http.FS(sub))
}
